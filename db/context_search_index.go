package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
	"unicode"
)

const contextSearchIndexBuildLockKey int64 = 7337741103

var contextSearchIndexes = []struct{ name, definition string }{
	{"series_context_node_text_v1", `ON exploration_nodes USING gin (series_context_search_text_v1(payload) gin_trgm_ops)`},
	{"series_context_report_text_v1", `ON findings USING gin (lower(COALESCE(report,'')) gin_trgm_ops) WHERE COALESCE(report,'')<>''`},
	{"series_context_activity_text_v1", `ON activity USING gin (lower(COALESCE(detail,'')||chr(10)||COALESCE(summary,'')) gin_trgm_ops) WHERE node_id IS NOT NULL AND kind NOT IN ('thinking','usage')`},
}

// Build after HTTP startup, never in a task's wakeup path. A cancelled build
// may leave an invalid index; next startup repairs it concurrently. Failure
// keeps exact, timeout-bounded search available via the existing query path.
func (d *DB) BuildContextSearchIndexes(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, contextSearchIndexBuildLockKey).Scan(&locked); err != nil || !locked {
		return err
	}
	shared := false
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		failed := false
		for _, q := range []string{`RESET maintenance_work_mem`, `RESET max_parallel_maintenance_workers`, `RESET lock_timeout`} {
			if _, e := conn.ExecContext(cleanup, q); e != nil {
				failed = true
			}
		}
		if shared {
			if _, e := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock_shared($1)`, schemaMigrationLockKey); e != nil {
				failed = true
			}
		}
		if _, e := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock($1)`, contextSearchIndexBuildLockKey); e != nil {
			failed = true
		}
		// A cancelled cleanup must not return session locks/settings to the pool.
		if failed {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	// Avoid racing expression-function migrations in another instance.
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared($1)`, schemaMigrationLockKey).Scan(&shared); err != nil || !shared {
		return err
	}
	// A later application startup needs the exclusive migration lock. Yield
	// this optional build promptly instead of delaying startup for ten minutes.
	go d.cancelIndexBuildForMigration(ctx, cancel)

	for _, setting := range []string{`SET maintenance_work_mem='32MB'`, `SET max_parallel_maintenance_workers=0`, `SET lock_timeout='1s'`} {
		if _, err = conn.ExecContext(ctx, setting); err != nil {
			return err
		}
	}
	for _, idx := range contextSearchIndexes {
		var valid bool
		e := conn.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, idx.name).Scan(&valid)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if valid {
			continue
		}
		if e == nil {
			if _, err = conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY `+idx.name); err != nil {
				return err
			}
		}
		if _, err = conn.ExecContext(ctx, `CREATE INDEX CONCURRENTLY `+idx.name+` `+idx.definition); err != nil {
			return fmt.Errorf("build %s: %w", idx.name, err)
		}
	}
	return nil
}

func (d *DB) cancelIndexBuildForMigration(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check, c := context.WithTimeout(ctx, time.Second)
			var waiting bool
			err := d.QueryRowContext(check, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND mode='ExclusiveLock' AND NOT granted)`, schemaMigrationLockKey>>32, schemaMigrationLockKey&0xffffffff).Scan(&waiting)
			c()
			if err != nil || waiting {
				cancel()
				return
			}
		}
	}
}

func (s *ExplorationStore) contextTextIndexed(ctx context.Context, q string) (bool, error) {
	// Substring patterns without three consecutive word characters cannot
	// narrow an unanchored trigram scan. Keep the existing path in that case.
	if !contextHasTrigram(q) {
		return false, nil
	}
	names := make([]string, 0, len(contextSearchIndexes))
	for _, i := range contextSearchIndexes {
		names = append(names, i.name)
	}
	var ready bool
	err := s.db.QueryRowContext(ctx, `SELECT count(*)=$2 FROM unnest($1::text[])n JOIN pg_index i ON i.indexrelid=to_regclass(n) WHERE i.indisvalid AND i.indisready`, names, len(names)).Scan(&ready)
	return ready, err
}

func contextHasTrigram(q string) bool {
	consecutive := 0
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			consecutive++
			if consecutive >= 3 {
				return true
			}
		} else {
			consecutive = 0
		}
	}
	return false
}

const contextNodeTextCandidates = ` AND n.id IN(
 SELECT i.id FROM exploration_nodes i WHERE i.exploration_id IN(SELECT exp FROM contexts)
 AND series_context_search_text_v1(i.payload) LIKE series_context_literal_pattern_v1($3) ESCAPE chr(92)
 UNION SELECT f.node_id FROM findings f JOIN exploration_nodes i ON i.id=f.node_id
 WHERE i.exploration_id IN(SELECT exp FROM contexts) AND COALESCE(f.report,'')<>'' AND lower(COALESCE(f.report,'')) LIKE series_context_literal_pattern_v1($3) ESCAPE chr(92))`
const contextActivityTextCandidates = ` AND a.id IN(SELECT ai.id FROM activity ai WHERE ai.exploration_id IN(SELECT exp FROM contexts) AND ai.node_id IS NOT NULL AND ai.kind NOT IN ('thinking','usage')
 AND lower(COALESCE(ai.detail,'')||chr(10)||COALESCE(ai.summary,'')) LIKE series_context_literal_pattern_v1($3) ESCAPE chr(92))`
