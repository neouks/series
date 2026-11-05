package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

func archiveStreamObject(raw json.RawMessage) (map[string]any, error) {
	var row map[string]any
	if err := decodeContextJSON(raw, &row); err != nil || row == nil {
		return nil, errors.New("invalid archive object")
	}
	return row, nil
}

func restoreCheckpointStream(ctx context.Context, tx *sql.Tx, reader io.Reader, expID, expected int64) (int64, error) {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO agent_execution_checkpoints(exploration_id,role,intent_id,version,body,sources,access_revision,context_stamp,saved_at)
 SELECT p.exploration_id,p.role,p.intent_id,p.version,COALESCE(p.body,'null'::jsonb),COALESCE(p.sources,'null'::jsonb),p.access_revision,p.context_stamp,p.saved_at
 FROM json_populate_recordset(NULL::agent_execution_checkpoints,$1::json)p
 WHERE p.role='planner' OR NOT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=p.intent_id AND (n.exploration_id<>p.exploration_id OR n.kind<>'intent'))`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	batch := newArchiveStreamBatch(stmt)
	r := newArchiveObjectReader(reader, maxArchiveStreamRowBytes)
	var count int64
	for {
		raw, err := r.Next(ctx)
		if errors.Is(err, io.EOF) {
			return count, batch.Flush(ctx)
		}
		if err != nil {
			return 0, err
		}
		if count >= expected {
			return 0, errors.New("checkpoint stream exceeds manifest count")
		}
		row, err := archiveStreamObject(raw)
		if err != nil {
			return 0, err
		}
		owner, ok := jsonInt64(row["exploration_id"])
		if !ok || owner != expID {
			return 0, errors.New("checkpoint exploration does not match archive")
		}
		intent, ok := jsonInt64(row["intent_id"])
		role, _ := row["role"].(string)
		if !ok || !validCheckpointOwner(role, intent) {
			return 0, errors.New("invalid checkpoint owner")
		}
		// Deleted Workers may leave historical checkpoints without a raw node.
		// Preserve those as stale notes; never bind to an existing foreign node.
		if row["context_stamp"] == nil {
			row["context_stamp"] = ""
		}
		raw, err = marshalArchiveObject(row)
		if err != nil {
			return 0, err
		}
		if err = batch.Add(ctx, raw); err != nil {
			return 0, err
		}
		count++
	}
}

func restoreLLMRecordStream(ctx context.Context, tx *sql.Tx, reader io.Reader, taskID, expected int64) (int64, error) {
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO llm_records SELECT p.* FROM json_populate_recordset(NULL::llm_records,$1::json)p WHERE p.task_id=$2`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	batch := newArchiveStreamBatch(stmt, strconv.FormatInt(taskID, 10))
	batch.rowLimit = 0 // Preserve legacy single-record capacity; source rows stay immutable.
	decoder := json.NewDecoder(reader)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return count, batch.Flush(ctx)
		}
		if err != nil {
			return 0, err
		}
		if count >= expected {
			return 0, errors.New("LLM stream exceeds manifest count")
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || raw[0] != '{' {
			return 0, errors.New("streamed LLM record must be a JSON object")
		}
		// Scope is checked against the same typed SQL row actually inserted,
		// so casing/duplicate-key semantics cannot differ between two parsers.
		if err = batch.Add(ctx, raw); err != nil {
			return 0, err
		}
		count++
	}
}

func restorePlannerWorkingStateStream(ctx context.Context, tx *sql.Tx, reader io.Reader, expID int64, expected ...int64) (int64, error) {
	r := newArchiveObjectReader(reader, maxArchiveStreamRowBytes)
	raw, err := r.Next(ctx)
	if errors.Is(err, io.EOF) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if len(expected) > 0 && expected[0] != 1 {
		return 0, errors.New("planner-state stream exceeds manifest count")
	}
	var header struct {
		Kind    string
		Row     json.RawMessage
		IsArray *bool  `json:"is_array"`
		Count   *int64 `json:"todo_count"`
		Value   json.RawMessage
	}
	if err = json.Unmarshal(raw, &header); err != nil {
		return 0, err
	}
	if header.Kind != "state" || header.IsArray == nil || header.Count == nil || *header.Count < 0 {
		return 0, errors.New("invalid planner-state stream header")
	}
	row, err := archiveStreamObject(header.Row)
	if err != nil {
		return 0, err
	}
	owner, ok := jsonInt64(row["exploration_id"])
	if !ok || owner != expID {
		return 0, errors.New("planner state exploration does not match archive")
	}
	if _, exists := row["todos"]; exists {
		return 0, errors.New("inline todos are not allowed in state stream header")
	}
	header.Row, err = marshalArchiveObject(row)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE series_archive_planner_todos(position BIGINT PRIMARY KEY,value JSONB NOT NULL) ON COMMIT DROP`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO series_archive_planner_todos(position,value)
 SELECT position,COALESCE(value,'null'::jsonb) FROM jsonb_to_recordset($1::jsonb)x(position bigint,value jsonb)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	batch := newArchiveStreamBatch(stmt)
	var count int64
	for {
		raw, err = r.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		var item struct {
			Kind  string
			Index *int64
			Value json.RawMessage
		}
		if err = json.Unmarshal(raw, &item); err != nil {
			return 0, err
		}
		if !*header.IsArray || item.Kind != "todo" || item.Index == nil || *item.Index != count || len(item.Value) == 0 || count >= *header.Count {
			return 0, errors.New("planner-state items are missing, duplicated or out of order")
		}
		encoded, e := marshalArchiveObject(struct {
			Position int64           `json:"position"`
			Value    json.RawMessage `json:"value"`
		}{count, item.Value})
		if e != nil {
			return 0, e
		}
		if err = batch.Add(ctx, encoded); err != nil {
			return 0, err
		}
		count++
	}
	if count != *header.Count {
		return 0, errors.New("planner-state item count does not match header")
	}
	if err = batch.Flush(ctx); err != nil {
		return 0, err
	}
	value := "null"
	if !*header.IsArray {
		if *header.Count != 0 || len(header.Value) == 0 {
			return 0, errors.New("invalid raw planner-state value")
		}
		value = string(header.Value)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planner_working_state(exploration_id,todos,revision)
 SELECT p.exploration_id,CASE WHEN $2 THEN COALESCE((SELECT jsonb_agg(value ORDER BY position) FROM series_archive_planner_todos),'[]'::jsonb)
 ELSE $3::jsonb END,p.revision FROM json_populate_record(NULL::planner_working_state,$1::json)p`, string(header.Row), *header.IsArray, value); err != nil {
		return 0, err
	}
	return 1, nil
}

// Retained internal helper for callers which need only a whitelisted LLM table.
func insertArchiveJSONSequenceRowsContext(ctx context.Context, tx *sql.Tx, table string, reader io.Reader) (int64, error) {
	if table != "llm_records" {
		return 0, fmt.Errorf("archive streamed restore table %q is not allowed", table)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO llm_records SELECT * FROM json_populate_record(NULL::llm_records,$1::json)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	r := newArchiveObjectReader(reader, maxArchiveStreamRowBytes)
	var count int64
	for {
		raw, e := r.Next(ctx)
		if errors.Is(e, io.EOF) {
			return count, nil
		}
		if e != nil {
			return 0, e
		}
		if _, e = stmt.ExecContext(ctx, string(raw)); e != nil {
			return 0, e
		}
		count++
	}
}
