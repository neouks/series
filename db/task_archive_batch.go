package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Keep restore round trips bounded as well as memory. A row larger than 256KiB
// is sent alone. Working-state frames have a 64MiB ceiling; LLM records keep
// their existing single-record capacity for compatibility with v2 archives.
type archiveStreamBatch struct {
	exec     func(context.Context, []byte) (int64, error)
	rows     []json.RawMessage
	size     int
	rowLimit int
}

func newArchiveStreamBatch(stmt *sql.Stmt, args ...any) *archiveStreamBatch {
	return &archiveStreamBatch{rowLimit: maxArchiveStreamRowBytes, exec: func(ctx context.Context, raw []byte) (int64, error) {
		params := append([]any{string(raw)}, args...)
		result, err := stmt.ExecContext(ctx, params...)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}}
}
func (b *archiveStreamBatch) Add(ctx context.Context, raw json.RawMessage) error {
	if b.rowLimit > 0 && len(raw) > b.rowLimit {
		return fmt.Errorf("archive stream row exceeds 64MiB")
	}
	if len(b.rows) > 0 && (len(b.rows) >= 32 || b.size+len(raw)+3 > 256<<10) {
		if err := b.Flush(ctx); err != nil {
			return err
		}
	}
	b.rows = append(b.rows, raw)
	b.size += len(raw) + 1
	// Release a full batch (especially a single large legacy record) before
	// the caller decodes the next frame and temporarily doubles its working set.
	if len(b.rows) >= 32 || b.size+1 >= 256<<10 {
		return b.Flush(ctx)
	}
	return nil
}
func (b *archiveStreamBatch) Flush(ctx context.Context) error {
	if len(b.rows) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw := make([]byte, 0, b.size+2)
	raw = append(raw, '[')
	for i, row := range b.rows {
		if i > 0 {
			raw = append(raw, ',')
		}
		raw = append(raw, row...)
	}
	raw = append(raw, ']')
	count, err := b.exec(ctx, raw)
	if err != nil {
		return err
	}
	if count != int64(len(b.rows)) {
		return fmt.Errorf("archive batch row count or scope mismatch")
	}
	clear(b.rows)
	b.rows = b.rows[:0]
	b.size = 0
	return nil
}
