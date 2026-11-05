package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// A callback owns just one decoded snapshot. It must not retain all headers or
// bodies; archive files can be reopened for later metadata restoration.
func ForEachArchiveEvidenceSnapshot(ctx context.Context, snapshot *TaskArchiveSnapshot, reader io.Reader, visit func(TrafficEvidenceSnapshot) error) error {
	if visit == nil {
		return errors.New("nil evidence snapshot consumer")
	}
	if err := ValidateTaskArchiveStreams(snapshot); err != nil {
		return err
	}
	return eachArchiveEvidenceRow(ctx, snapshot, "traffic_evidence_snapshots", reader, func(raw json.RawMessage) error {
		var v TrafficEvidenceSnapshot
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if v.ID != TrafficSnapshotID(v) || v.ReqLen < 0 || v.RespLen < 0 {
			return errors.New("archived evidence snapshot hash or length is invalid")
		}
		return visit(v)
	})
}

func eachArchiveEvidenceRow(ctx context.Context, snapshot *TaskArchiveSnapshot, table string, reader io.Reader, visit func(json.RawMessage) error) error {
	if !archiveEvidenceStreamTable(table) {
		return errors.New("unsupported evidence stream")
	}
	streamed := snapshot.StreamedTables[table] != ""
	count := int64(0)
	accept := func(raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if snapshot.FormatVersion < 3 {
			return ErrTaskArchiveFormatMismatch
		}
		if expected, ok := snapshot.DataCounts[table]; ok && count >= expected {
			return errors.New("evidence stream exceeds manifest count")
		}
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			return errors.New("archived evidence row must be an object")
		}
		if err := visit(raw); err != nil {
			return err
		}
		count++
		return nil
	}
	if streamed {
		if reader == nil {
			return fmt.Errorf("missing evidence stream %s", table)
		}
		r := newArchiveObjectReader(reader, maxArchiveStreamRowBytes)
		for {
			raw, err := r.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			var frame struct {
				Row   json.RawMessage `json:"row"`
				Nulls *[]string       `json:"sql_nulls"`
			}
			if err = json.Unmarshal(raw, &frame); err != nil {
				return err
			}
			// These two schemas have no JSON columns. A nonempty/null directory
			// cannot be used to change the actual value inserted by the restore.
			if frame.Nulls == nil || len(*frame.Nulls) != 0 {
				return errors.New("invalid evidence SQL-null directory")
			}
			if err = accept(frame.Row); err != nil {
				return err
			}
		}
	} else {
		raw := bytes.TrimSpace(snapshot.Tables[table])
		if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			d := json.NewDecoder(bytes.NewReader(raw))
			if token, err := d.Token(); err != nil || token != json.Delim('[') {
				return errors.New("archived evidence table must be an array")
			}
			for d.More() {
				var row json.RawMessage
				if err := d.Decode(&row); err != nil {
					return err
				}
				if err := accept(row); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil {
				return err
			}
			if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
				return errors.New("trailing evidence archive data")
			}
		}
	}
	if expected, ok := snapshot.DataCounts[table]; ok && expected != count {
		return fmt.Errorf("evidence stream count %d differs from manifest %d", count, expected)
	}
	return ctx.Err()
}

func restoreFindingTrafficStreams(ctx context.Context, tx *sql.Tx, snapshot *TaskArchiveSnapshot, streams map[string]io.Reader) error {
	initialized := false
	batch := &archiveStreamBatch{rowLimit: maxArchiveStreamRowBytes, exec: func(ctx context.Context, raw []byte) (int64, error) {
		var n int64
		err := tx.QueryRowContext(ctx, `WITH incoming AS MATERIALIZED (SELECT p.* FROM jsonb_populate_recordset(NULL::traffic_evidence_snapshots,$1::jsonb)p),
 ids AS (INSERT INTO series_archive_evidence_ids SELECT id FROM incoming RETURNING id),
 saved AS (INSERT INTO traffic_evidence_snapshots(id,source_traffic_id,captured_at,url,method,status,content_type,req_head,resp_head,req_hash,resp_hash,req_len,resp_len,created_at)
 SELECT id,source_traffic_id,captured_at,url,method,status,content_type,req_head,resp_head,req_hash,resp_hash,req_len,resp_len,created_at FROM incoming ON CONFLICT(id) DO NOTHING RETURNING id)
 SELECT count(*) FROM ids`, string(raw)).Scan(&n)
		return n, err
	}}
	if snapshot.StreamedTables["traffic_evidence_snapshots"] == "" {
		batch.rowLimit = 0
	}
	err := ForEachArchiveEvidenceSnapshot(ctx, snapshot, streams["traffic_evidence_snapshots"], func(v TrafficEvidenceSnapshot) error {
		if !initialized {
			if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE series_archive_evidence_ids(id text PRIMARY KEY) ON COMMIT DROP`); err != nil {
				return err
			}
			initialized = true
		}
		// Insert the same canonical, validated value, avoiding case-variant or
		// duplicate JSON keys being interpreted differently by Go and PostgreSQL.
		v = v.Normalize()
		if v.CreatedAt.IsZero() {
			v.CreatedAt = time.Now().UTC()
		}
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		if err := e.Encode(v); err != nil {
			return err
		}
		return batch.Add(ctx, bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	})
	if err != nil {
		return err
	}
	if err = batch.Flush(ctx); err != nil {
		return err
	}
	if !initialized {
		// Empty/old tasks should not pay for temporary tables, schema lookup,
		// statement preparation or sequence changes. Still validate the binding
		// stream and its manifest count; missing data must not become empty.
		return eachArchiveEvidenceRow(ctx, snapshot, "finding_traffic_bindings", streams["finding_traffic_bindings"], func(json.RawMessage) error { return errors.New("binding has no archived evidence snapshot") })
	}
	columns, err := archiveDataColumns(ctx, tx, "finding_traffic_bindings")
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, archiveDataInsertSQL("finding_traffic_bindings", columns))
	if err != nil {
		return err
	}
	defer stmt.Close()
	bindings := newArchiveStreamBatch(stmt, snapshot.ExplorationID, snapshot.TaskID)
	if snapshot.StreamedTables["finding_traffic_bindings"] == "" {
		bindings.rowLimit = 0
	}
	err = eachArchiveEvidenceRow(ctx, snapshot, "finding_traffic_bindings", streams["finding_traffic_bindings"], func(raw json.RawMessage) error {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		if err := e.Encode(map[string]any{"row": raw, "sql_nulls": []string{}}); err != nil {
			return err
		}
		return bindings.Add(ctx, bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	})
	if err != nil {
		return err
	}
	if err = bindings.Flush(ctx); err != nil {
		return err
	}
	var unused bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM series_archive_evidence_ids i WHERE NOT EXISTS(SELECT 1 FROM finding_traffic_bindings b JOIN findings f ON f.id=b.finding_id WHERE b.snapshot_id=i.id AND f.task_id=$1))`, snapshot.TaskID).Scan(&unused); err != nil {
		return err
	}
	if unused {
		return errors.New("archive includes evidence outside task bindings")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE traffic_evidence_snapshots s SET unreferenced_at=NULL FROM series_archive_evidence_ids i WHERE s.id=i.id`); err != nil {
		return err
	}
	return advanceArchiveSequence(ctx, tx, "finding_traffic_bindings")
}

// Never lower a shared sequence when other tasks have allocated/deleted rows.
// Resolve its actual schema/name from the table's owned serial sequence.
func advanceArchiveSequence(ctx context.Context, tx *sql.Tx, table string) error {
	if table != "finding_traffic_bindings" && table != "mcp_usage" {
		return errors.New("unsupported archive sequence")
	}
	_, err := tx.ExecContext(ctx, `WITH sequence_state AS (
 SELECT max(s.last_value) last FROM pg_sequences s JOIN pg_namespace n ON n.nspname=s.schemaname
 JOIN pg_class c ON c.relnamespace=n.oid AND c.relname=s.sequencename WHERE c.oid=pg_get_serial_sequence($1,'id')::regclass
 ), restored AS (SELECT max(id) upper FROM `+archiveSQLIdent(table)+`)
 SELECT setval(pg_get_serial_sequence($1,'id'),GREATEST(COALESCE(upper,1),COALESCE(last,1)),last IS NOT NULL OR upper IS NOT NULL) FROM sequence_state,restored`, table)
	return err
}
