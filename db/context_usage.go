package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// These queries read the lightweight ledgers only, never activities, prompts,
// evidence or graph payloads. Explicit task attribution takes priority; only
// legacy rows without a task ID may use the owning exploration as a fallback.
const contextModelUsageSQL = `SELECT COALESCE(NULLIF(worker,''),'unknown') role,
 COALESCE(NULLIF(phase,''),'unknown') phase,count(*) calls,
 count(*) FILTER(WHERE status='error') failed_calls,
 count(*) FILTER(WHERE retry_ordinal>0) run_retry_calls,
 COALESCE(sum(input_tokens),0) input_tokens,COALESCE(sum(output_tokens),0) output_tokens,
 COALESCE(sum(cache_read),0) cache_read_tokens,COALESCE(sum(cache_write),0) cache_write_tokens,
 COALESCE(sum(latency_ms),0) latency_ms,
 COALESCE(sum(system_chars),0) system_chars,COALESCE(sum(message_chars),0) message_chars,
 COALESCE(sum(tool_schema_chars),0) tool_schema_chars,COALESCE(sum(request_chars),0) request_chars,
 COALESCE(sum(request_bytes),0) request_bytes,max(request_bytes) peak_request_bytes,
 count(*) FILTER(WHERE request_bytes IS NOT NULL) byte_observed_calls,
 count(*) FILTER(WHERE history_read_calls IS NOT NULL) history_observed_calls,
 COALESCE(sum(history_read_calls),0) history_read_calls,
 COALESCE(sum(history_repeated_reads),0) history_repeated_reads,
 count(*) FILTER(WHERE history_read_tracking_limited) history_tracking_limited_calls
 FROM llm_usage WHERE task_id=$1 OR (NULLIF(task_id,'') IS NULL AND exploration_id=$2)
 GROUP BY 1,2`

var contextToolUsageSQL = makeContextToolUsageSQL()

func makeContextToolUsageSQL() string {
	current := map[string]bool{"context_search": true, "context_get": true, "asset_search": true, "asset_get": true, "traffic_search": true, "traffic_get": true, "graph_overview": true}
	keys := map[string]bool{}
	for key := range current {
		keys[key] = true
	}
	for key := range RetiredToolReplacements {
		if current[CanonicalToolUsageKey(key)] {
			keys[key] = true
		}
	}
	keys["get_worker_trace"], keys["get_task_worker_trace"] = true, true
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	quoted, arms := []string{}, []string{}
	for _, key := range ordered {
		quoted = append(quoted, archiveSQLLiteral(key))
		arms = append(arms, "WHEN "+archiveSQLLiteral(key)+" THEN "+archiveSQLLiteral(CanonicalToolUsageKey(key)))
	}
	return `SELECT COALESCE(NULLIF(agent_key,''),'unknown') role,CASE tool_key ` + strings.Join(arms, " ") + ` END tool,count(*) calls
 FROM tool_usage WHERE (task_id=$1::bigint OR (task_id IS NULL AND exploration_id=$2))
 AND tool_key IN (` + strings.Join(quoted, ",") + `) GROUP BY 1,2`
}

type ContextUsagePage struct {
	Items      []json.RawMessage `json:"items"`
	HasMore    bool              `json:"has_more"`
	NextOffset *int              `json:"next_offset,omitempty"`
}

type TaskContextUsage struct {
	Source     string           `json:"source"`
	ModelCalls ContextUsagePage `json:"model_calls"`
	ToolReads  ContextUsagePage `json:"tool_reads"`
}

// Paginate model-view metrics and actual tool executions separately: summing
// history_read_calls does not mean the tools executed that many times.
func (d *DB) TaskContextUsage(ctx context.Context, taskID int64, offset, limit int) (TaskContextUsage, error) {
	out := TaskContextUsage{Source: "live"}
	if taskID <= 0 || offset < 0 || offset > 1000000 || limit < 1 || limit > 200 {
		return out, fmt.Errorf("invalid context usage page")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var expID int64
	var archived bool
	var cold []byte
	err = tx.QueryRowContext(ctx, `SELECT t.exploration_id,t.archived_at IS NOT NULL,
 CASE WHEN t.archived_at IS NOT NULL THEN a.aggregate_stats->'context_usage' END
 FROM tasks t LEFT JOIN task_archives a ON a.task_id=t.id
 WHERE t.id=$1 AND (t.deleted_at IS NULL OR t.archived_at IS NOT NULL)`, taskID).Scan(&expID, &archived, &cold)
	if err != nil {
		return out, err
	}
	q := archiveContextQuery{ctx: ctx, tx: tx}
	for _, part := range []struct {
		name, query string
		dst         *ContextUsagePage
	}{
		{"model_calls", contextModelUsageSQL, &out.ModelCalls}, {"tool_reads", contextToolUsageSQL, &out.ToolReads},
	} {
		query := `SELECT to_jsonb(x) FROM (` + part.query + `)x ORDER BY role,` + map[bool]string{true: "phase", false: "tool"}[part.name == "model_calls"] + ` LIMIT $3 OFFSET $4`
		args := []any{strconv.FormatInt(taskID, 10), expID, limit + 1, offset}
		if archived {
			out.Source = "archive"
			if len(cold) == 0 || string(cold) == "null" {
				out.Source = "archive_metrics_unavailable"
			}
			query = `SELECT item FROM jsonb_array_elements(COALESCE($1::jsonb->$2,'[]'::jsonb))item ORDER BY item->>'role',item->>'` + map[bool]string{true: "phase", false: "tool"}[part.name == "model_calls"] + `' LIMIT $3 OFFSET $4`
			var payload any
			if len(cold) > 0 {
				payload = string(cold)
			}
			args = []any{payload, part.name, limit + 1, offset}
		}
		rows, e := q.Query(query, args...)
		if e != nil {
			return out, e
		}
		part.dst.Items = []json.RawMessage{}
		for rows.Next() {
			var raw json.RawMessage
			if e = rows.Scan(&raw); e != nil {
				break
			}
			part.dst.Items = append(part.dst.Items, raw)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return out, e
		}
		if len(part.dst.Items) > limit {
			part.dst.HasMore = true
			part.dst.Items = part.dst.Items[:limit]
			next := offset + limit
			part.dst.NextOffset = &next
		}
	}
	return out, tx.Commit()
}

func snapshotContextUsage(q archiveSnapshotQueryer, taskID, expID int64) (json.RawMessage, error) {
	var raw json.RawMessage
	err := q.QueryRow(`SELECT jsonb_build_object('model_calls',
 (SELECT COALESCE(jsonb_agg(to_jsonb(x)),'[]'::jsonb) FROM (`+contextModelUsageSQL+`)x),
 'tool_reads',(SELECT COALESCE(jsonb_agg(to_jsonb(x)),'[]'::jsonb) FROM (`+contextToolUsageSQL+`)x))`, strconv.FormatInt(taskID, 10), expID).Scan(&raw)
	return raw, err
}
