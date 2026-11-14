package db

import (
	"context"
	"encoding/json"
	"slices"
	"time"
)

// DBLog is one persisted backend log row from the server_logs table.
type DBLog struct {
	ID        int64
	CreatedAt time.Time
	Level     string
	Tag       string
	Text      string
	Fields    json.RawMessage
}

// InsertLog appends one log line and returns its auto-assigned id.
func (d *DB) InsertLog(ctx context.Context, line DBLog) (int64, error) {
	fields := line.Fields
	if len(fields) == 0 {
		fields = json.RawMessage("{}")
	}
	var id int64
	err := d.QueryRowContext(ctx,
		"INSERT INTO server_logs(created_at,level,tag,text,fields) VALUES($1,$2,$3,$4,$5) RETURNING id",
		line.CreatedAt, line.Level, line.Tag, line.Text, fields,
	).Scan(&id)
	return id, err
}

// RecentLogs returns the most recent limit rows, oldest-first.
func (d *DB) RecentLogs(ctx context.Context, limit int) ([]*DBLog, error) {
	return d.queryLogs(ctx,
		`SELECT id, created_at, level, tag, text, fields
		   FROM server_logs
		  ORDER BY id DESC
		  LIMIT $1`, limit)
}

// ListLogsBefore returns up to limit rows with id < beforeID, oldest-first.
func (d *DB) ListLogsBefore(ctx context.Context, beforeID int64, limit int) ([]*DBLog, error) {
	return d.queryLogs(ctx,
		`SELECT id, created_at, level, tag, text, fields
		   FROM server_logs
		  WHERE id < $1
		  ORDER BY id DESC
		  LIMIT $2`, beforeID, limit)
}

func (d *DB) queryLogs(ctx context.Context, query string, args ...any) ([]*DBLog, error) {
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DBLog
	for rows.Next() {
		l := &DBLog{}
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.Level, &l.Tag, &l.Text, &l.Fields); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}
