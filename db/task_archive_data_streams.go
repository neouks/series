package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Source predicates and insert order continue to come from the existing task
// snapshot/restore service. Only these tables may use the v5 row envelope.
// p is the same typed record actually inserted, not a separately decoded ID.
var archiveDataStreamScopes = map[string]string{
	"exploration_nodes":         `p.exploration_id=$2`,
	"exploration_edges":         `p.exploration_id=$2 AND EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.src_id AND n.exploration_id=$2) AND EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.dst_id AND n.exploration_id=$2)`,
	"exploration_anchors":       `EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.node_id AND n.exploration_id=$2)`,
	"task_constraints":          `p.exploration_id=$2`,
	"activity":                  `p.exploration_id=$2 AND NOT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.node_id AND n.exploration_id<>$2)`,
	"main_sessions":             `p.exploration_id=$2`,
	"deleted_workers":           `p.exploration_id=$2`,
	"findings":                  `p.task_id=$3 AND NOT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.node_id AND n.exploration_id<>$2)`,
	"finding_retests":           `EXISTS(SELECT 1 FROM findings f WHERE f.id=p.finding_id AND f.task_id=$3)`,
	"finding_deletion_feedback": `p.task_id=$3`,
	"finding_suppression_rules": `p.task_id=$3 AND EXISTS(SELECT 1 FROM finding_deletion_feedback d WHERE d.id=p.feedback_id AND d.task_id=$3)`,
	"llm_usage":                 `p.task_id=($3::bigint)::text OR p.exploration_id=$2`,
	"skill_usage":               `p.task_id=$3 OR p.exploration_id=$2`,
	"tool_usage":                `p.task_id=$3 OR p.exploration_id=$2`,
	"mcp_usage":                 `p.task_id=$3 OR p.exploration_id=$2`,
	"side_question_sessions":    `p.task_id=$3`,
	"side_question_requests":    `EXISTS(SELECT 1 FROM side_question_sessions s WHERE s.session_key=p.session_key AND s.task_id=$3)`,
	// Snapshot rows require hash validation and archive-local ID tracking;
	// the generic insertion path is intentionally disabled for that table.
	"traffic_evidence_snapshots": `false`,
	"finding_traffic_bindings":   `p.id>0 AND p.position>=0 AND EXISTS(SELECT 1 FROM findings f WHERE f.id=p.finding_id AND f.task_id=$3) AND EXISTS(SELECT 1 FROM series_archive_evidence_ids i WHERE i.id=p.snapshot_id) AND p.role IN ('baseline','proof','verification','supporting')`,
}

func archiveEvidenceStreamTable(name string) bool {
	return name == "traffic_evidence_snapshots" || name == "finding_traffic_bindings"
}

func archiveDataStreamTable(name string) bool { _, ok := archiveDataStreamScopes[name]; return ok }
func archiveSQLIdent(s string) string         { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func archiveSQLLiteral(s string) string       { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

type archiveDataColumn struct {
	name, kind string
	notNull    bool
}

func archiveDataColumns(ctx context.Context, tx *sql.Tx, table string) ([]archiveDataColumn, error) {
	if !archiveDataStreamTable(table) {
		return nil, fmt.Errorf("unsupported archive table %q", table)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.attname,t.typname,a.attnotnull FROM pg_attribute a JOIN pg_type t ON t.oid=a.atttypid
 WHERE a.attrelid=$1::regclass AND a.attnum>0 AND NOT a.attisdropped AND a.attgenerated='' ORDER BY a.attnum`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []archiveDataColumn
	for rows.Next() {
		var c archiveDataColumn
		if err = rows.Scan(&c.name, &c.kind, &c.notNull); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func streamArchiveDataRows(ctx context.Context, tx *sql.Tx, w io.Writer, table, inner string, args ...any) (int64, error) {
	columns, err := archiveDataColumns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	nulls := []string{}
	for _, c := range columns {
		if (c.kind == "jsonb" || c.kind == "json") && !c.notNull {
			nulls = append(nulls, `CASE WHEN row_data.`+archiveSQLIdent(c.name)+` IS NULL THEN `+archiveSQLLiteral(c.name)+` END`)
		}
	}
	query := `SELECT jsonb_build_object('row',to_jsonb(row_data),'sql_nulls',to_jsonb(array_remove(ARRAY[` + strings.Join(nulls, ",") + `]::text[],NULL)))::text line FROM (` + inner + `)row_data`
	return streamArchiveObjectQuery(ctx, tx, w, query, args...)
}

// Nullable JSON fields need an explicit SQL-null directory. row_to_json alone
// cannot distinguish a SQL NULL from the JSON value null. Use the original
// envelope for those columns rather than json_populate_record's null coercion.
func archiveDataInsertSQL(table string, columns []archiveDataColumn) string {
	names, values := []string{}, []string{}
	for _, c := range columns {
		names = append(names, archiveSQLIdent(c.name))
		value := `p.` + archiveSQLIdent(c.name)
		if c.kind == "jsonb" || c.kind == "json" {
			value = `CASE WHEN (f.data->'sql_nulls') ? ` + archiveSQLLiteral(c.name) + ` THEN NULL ELSE (f.data->'row'->` + archiveSQLLiteral(c.name) + `)::` + c.kind + ` END`
		}
		if table == "finding_retests" && c.name == "conversation_id" {
			value = `CASE WHEN EXISTS(SELECT 1 FROM conversations c WHERE c.id=p.conversation_id) THEN p.conversation_id ELSE NULL END`
		}
		values = append(values, value)
	}
	return `INSERT INTO ` + archiveSQLIdent(table) + `(` + strings.Join(names, ",") + `) SELECT ` + strings.Join(values, ",") + `
 FROM jsonb_array_elements($1::jsonb)f(data) CROSS JOIN LATERAL jsonb_populate_record(NULL::` + archiveSQLIdent(table) + `,f.data->'row')p
 WHERE (` + archiveDataStreamScopes[table] + `) AND $2::bigint>0 AND $3::bigint>0`
}

func restoreArchiveDataStream(ctx context.Context, tx *sql.Tx, table string, r io.Reader, expID, taskID, expected int64, mapping map[int64]int64) (int64, error) {
	reader := newArchiveObjectReader(r, maxArchiveStreamRowBytes)
	first, err := reader.Next(ctx)
	if errors.Is(err, io.EOF) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if expected == 0 {
		return 0, errors.New("archive data stream exceeds manifest count")
	}
	columns, err := archiveDataColumns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	jsonColumns := map[string]bool{}
	for _, c := range columns {
		if c.kind == "jsonb" || c.kind == "json" {
			jsonColumns[c.name] = !c.notNull
		}
	}
	stmt, err := tx.PrepareContext(ctx, archiveDataInsertSQL(table, columns))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	batch := newArchiveStreamBatch(stmt, expID, taskID)
	var count int64
	raw := first
	for {
		if count >= expected {
			return 0, errors.New("archive data stream exceeds manifest count")
		}
		var frame struct {
			Row   json.RawMessage `json:"row"`
			Nulls *[]string       `json:"sql_nulls"`
		}
		if err = json.Unmarshal(raw, &frame); err != nil {
			return 0, err
		}
		if frame.Nulls == nil {
			return 0, errors.New("archive row has no SQL-null directory")
		}
		var row map[string]json.RawMessage
		if err = json.Unmarshal(frame.Row, &row); err != nil || row == nil {
			return 0, errors.New("invalid archived row object")
		}
		seen := map[string]bool{}
		for _, name := range *frame.Nulls {
			if !jsonColumns[name] || seen[name] || !bytes.Equal(bytes.TrimSpace(row[name]), []byte("null")) {
				return 0, errors.New("invalid archive SQL-null directory")
			}
			seen[name] = true
		}
		if err = normalizeArchiveDataRow(table, row, mapping); err != nil {
			return 0, err
		}
		frame.Row, err = marshalArchiveObject(row)
		if err != nil {
			return 0, err
		}
		raw, err = marshalArchiveObject(frame)
		if err != nil {
			return 0, err
		}
		if err = batch.Add(ctx, raw); err != nil {
			return 0, err
		}
		count++
		raw, err = reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			return count, batch.Flush(ctx)
		}
		if err != nil {
			return 0, err
		}
	}
}

func normalizeArchiveDataRow(table string, row map[string]json.RawMessage, mapping map[int64]int64) error {
	if table == "exploration_nodes" {
		if len(row["queue_position"]) == 0 || bytes.Equal(row["queue_position"], []byte("null")) {
			row["queue_position"] = json.RawMessage(`0`)
		}
		var payload map[string]json.RawMessage
		if bytes.HasPrefix(bytes.TrimSpace(row["payload"]), []byte("{")) {
			if err := json.Unmarshal(row["payload"], &payload); err != nil {
				return err
			}
			if err := remapArchiveAssetArray(payload, mapping); err != nil {
				return err
			}
			raw, err := marshalArchiveObject(payload)
			if err != nil {
				return err
			}
			row["payload"] = raw
		}
	}
	if table == "findings" || table == "finding_suppression_rules" {
		if err := remapArchiveAssetArray(row, mapping); err != nil {
			return err
		}
	}
	if table == "exploration_anchors" {
		var id any
		if err := decodeContextJSON(row["asset_id"], &id); err != nil {
			return err
		}
		if old, ok := jsonInt64(id); ok {
			if current, exists := mapping[old]; exists {
				raw, _ := json.Marshal(current)
				row["asset_id"] = raw
			}
		}
	}
	if table == "findings" {
		for _, key := range []string{"evidence_version", "report_evidence_version"} {
			if len(row[key]) == 0 || bytes.Equal(row[key], []byte("null")) {
				row[key] = json.RawMessage(`0`)
			}
		}
	}
	if table == "finding_retests" {
		var status string
		_ = json.Unmarshal(row["status"], &status)
		if status == "pending" || status == "running" {
			row["status"] = json.RawMessage(`"stopped"`)
			row["error"], _ = json.Marshal("任务归档期间复测已停止")
			row["finished_at"], _ = json.Marshal(time.Now().UTC())
		}
	}
	return nil
}

func remapArchiveAssetArray(row map[string]json.RawMessage, mapping map[int64]int64) error {
	if !bytes.HasPrefix(bytes.TrimSpace(row["asset_ids"]), []byte("[")) {
		return nil
	}
	var values []any
	if err := decodeContextJSON(row["asset_ids"], &values); err != nil {
		return err
	}
	for i, value := range values {
		if old, ok := jsonInt64(value); ok {
			if current, exists := mapping[old]; exists {
				values[i] = current
			}
		}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	row["asset_ids"] = raw
	return nil
}
