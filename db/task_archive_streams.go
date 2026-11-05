package db

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

const (
	TaskArchiveCheckpointsPath  = "database/agent_execution_checkpoints.ndjson"
	TaskArchivePlannerStatePath = "database/planner_working_state.ndjson"
	maxArchiveStreamRowBytes    = 64 << 20
)

func TaskArchiveStreamFiles() map[string]string {
	files := map[string]string{"llm_records": TaskArchiveLLMRecordsPath, "agent_execution_checkpoints": TaskArchiveCheckpointsPath, "planner_working_state": TaskArchivePlannerStatePath}
	for name := range archiveDataStreamScopes {
		files[name] = "database/" + name + ".ndjson"
	}
	return files
}

// Exact paths only; never turn untrusted manifest names into arbitrary files or
// tables. A streamed table cannot also contain an inline body.
func ValidateTaskArchiveStreams(snapshot *TaskArchiveSnapshot) error {
	if snapshot == nil || !IsTaskArchiveFormatSupported(snapshot.FormatVersion) {
		return ErrTaskArchiveFormatMismatch
	}
	paths := TaskArchiveStreamFiles()
	for name, path := range snapshot.StreamedTables {
		minimum := 4
		if name == "llm_records" {
			minimum = 2
		} else if archiveEvidenceStreamTable(name) {
			minimum = 6
		} else if archiveDataStreamTable(name) {
			minimum = 5
		}
		if paths[name] == "" || paths[name] != path || snapshot.FormatVersion < minimum {
			return fmt.Errorf("%w: unsupported streamed table metadata", ErrTaskArchiveFormatMismatch)
		}
		raw := bytes.TrimSpace(snapshot.Tables[name])
		if len(raw) > 0 && !bytes.Equal(raw, []byte("[]")) && !bytes.Equal(raw, []byte("null")) {
			return fmt.Errorf("%w: duplicate inline and streamed table %s", ErrTaskArchiveFormatMismatch, name)
		}
		count, ok := snapshot.DataCounts[name]
		if count < 0 || snapshot.FormatVersion >= 4 && !ok || name == "planner_working_state" && count > 1 {
			return fmt.Errorf("%w: missing or invalid stream count %s", ErrTaskArchiveFormatMismatch, name)
		}
	}
	return nil
}

type archiveContextQuery struct {
	ctx context.Context
	tx  *sql.Tx
}

func marshalArchiveObject(value any) (json.RawMessage, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
	if len(raw) > maxArchiveStreamRowBytes {
		return nil, errors.New("archive stream row exceeds 64MiB after normalization")
	}
	return raw, nil
}

type archiveSnapshotQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func (q archiveContextQuery) QueryRow(query string, args ...any) *sql.Row {
	return q.tx.QueryRowContext(q.ctx, query, args...)
}

func (q archiveContextQuery) Query(query string, args ...any) (*sql.Rows, error) {
	return q.tx.QueryContext(q.ctx, query, args...)
}

func writeArchiveObject(ctx context.Context, writer io.Writer, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxArchiveStreamRowBytes {
		return fmt.Errorf("archive stream row exceeds 64MiB; original task data preserved")
	}
	if n, err := writer.Write(raw); err != nil {
		return err
	} else if n != len(raw) {
		return io.ErrShortWrite
	}
	if n, err := io.WriteString(writer, "\n"); err != nil {
		return err
	} else if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func streamBoundedArchiveRows(ctx context.Context, tx *sql.Tx, writer io.Writer, inner string, args ...any) (int64, error) {
	return streamArchiveObjectQuery(ctx, tx, writer, `SELECT row_to_json(row_data)::text line FROM (`+inner+`)row_data`, args...)
}

func streamArchiveObjectQuery(ctx context.Context, tx *sql.Tx, writer io.Writer, query string, args ...any) (int64, error) {
	// Enforce the wire limit before a huge legacy row is copied into Go. SQL
	// serialization can still process that one source value inside PostgreSQL.
	args = append(args, maxArchiveStreamRowBytes)
	// OFFSET 0 keeps serialization in the inner iterator. Pulling it into the
	// CASE would encode every frame twice (for the size check and its value).
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN octet_length(x.line)<=$`+strconv.Itoa(len(args))+` THEN x.line ELSE NULL END FROM (`+query+` OFFSET 0)x`, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return 0, err
		}
		if err = writeArchiveObject(ctx, writer, raw); err != nil {
			return 0, err
		}
		count++
	}
	return count, rows.Err()
}

func streamPlannerWorkingState(ctx context.Context, tx *sql.Tx, writer io.Writer, expID int64) (int64, error) {
	count, err := streamArchiveObjectQuery(ctx, tx, writer, `SELECT jsonb_build_object('kind','state','row',jsonb_build_object('exploration_id',p.exploration_id,'revision',p.revision),
 'is_array',jsonb_typeof(todos)='array','todo_count',CASE WHEN jsonb_typeof(todos)='array' THEN jsonb_array_length(todos) ELSE 0 END,
 'value',CASE WHEN jsonb_typeof(todos)='array' THEN NULL ELSE todos END)::text line
 FROM planner_working_state p WHERE exploration_id=$1`, expID)
	if err != nil || count == 0 {
		return count, err
	}
	_, err = streamArchiveObjectQuery(ctx, tx, writer, `SELECT jsonb_build_object('kind','todo','index',a.ordinality-1,'value',a.value)::text line
 FROM planner_working_state p CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(p.todos)='array' THEN p.todos ELSE '[]'::jsonb END)
 WITH ORDINALITY a(value,ordinality) WHERE p.exploration_id=$1 ORDER BY a.ordinality`, expID)
	return count, err
}

// Bounded JSON-object sequence reader, also accepting legacy whitespace/adjacent
// object streams. No per-record reader chains or whole-file decoding. The
// caller supplies files in production; arbitrary blocking readers are not made
// forcibly interruptible by this cooperative context check.
type archiveObjectReader struct {
	reader *bufio.Reader
	limit  int
}

func newArchiveObjectReader(r io.Reader, limit int) *archiveObjectReader {
	return &archiveObjectReader{bufio.NewReaderSize(r, 32<<10), limit}
}
func (r *archiveObjectReader) Next(ctx context.Context) (json.RawMessage, error) {
	var raw []byte
	depth, quoted, escaped := 0, false, false
	for scanned := 0; ; scanned++ {
		if scanned%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		b, err := r.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(raw) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if len(raw) == 0 {
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
				continue
			}
			if b != '{' {
				return nil, errors.New("streamed archive row must be a JSON object")
			}
		}
		if len(raw) >= r.limit {
			return nil, fmt.Errorf("archive stream row exceeds %d bytes", r.limit)
		}
		raw = append(raw, b)
		if quoted {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		if depth == 0 {
			if !json.Valid(raw) {
				return nil, errors.New("invalid JSON in archive stream")
			}
			return raw, nil
		}
	}
}
