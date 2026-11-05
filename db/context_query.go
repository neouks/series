package db

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// Recursive visibility has deliberately conservative row estimates. Even a
// one-node read can cross PostgreSQL's JIT threshold and spend most of the
// five-second read budget compiling a plan. Scope jit=off to this read-only
// transaction; never leak a session setting into the shared pool.
func (s *ExplorationStore) contextReadTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL jit=off`); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

type contextRow struct {
	store *ExplorationStore
	ctx   context.Context
	query string
	args  []any
}

func (s *ExplorationStore) contextRow(ctx context.Context, query string, args ...any) contextRow {
	return contextRow{s, ctx, query, args}
}

func (r contextRow) Scan(dest ...any) error {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	tx, err := r.store.contextReadTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return tx.QueryRowContext(ctx, r.query, r.args...).Scan(dest...)
}

type contextRows struct {
	*sql.Rows
	tx         *sql.Tx
	cancel     context.CancelFunc
	afterClose func() error
	closeOnce  sync.Once
	closeErr   error
}

func (r *contextRows) Close() error {
	r.closeOnce.Do(func() {
		defer r.cancel()
		r.closeErr = r.Rows.Close()
		var err error
		if r.tx != nil {
			err = r.tx.Rollback()
			if err == sql.ErrTxDone {
				err = nil
			}
		}
		if r.afterClose != nil {
			err = r.afterClose()
		}
		if r.closeErr == nil {
			r.closeErr = err
		}
	})
	return r.closeErr
}

func (s *ExplorationStore) contextQuery(ctx context.Context, query string, args ...any) (*contextRows, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	tx, err := s.contextReadTx(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		tx.Rollback()
		cancel()
		return nil, err
	}
	return &contextRows{Rows: rows, tx: tx, cancel: cancel}, nil
}
