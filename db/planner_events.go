package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrPlannerWatermarkConflict = errors.New("planner watermark changed")

// PlannerEventSnapshot is a delivery boundary, not an authorization snapshot.
// Access is checked again when references are projected/read. Rows committed
// after Upper cannot be acknowledged by this planning round.
type PlannerEventSnapshot struct {
	ExplorationID    int64
	Generation       int64
	Lower, Upper     int64
	GraphRevision    string
	PreviousRevision string
}

func (s PlannerEventSnapshot) Pending() bool {
	return s.Upper > s.Lower || s.GraphRevision != s.PreviousRevision
}

// Read the graph vector and delivery boundary in one MVCC statement. Direct
// source changes affect the vector, without granting cross-task tool access.
func (s *ExplorationStore) PlannerEventSnapshot(ctx context.Context) (PlannerEventSnapshot, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	out := PlannerEventSnapshot{ExplorationID: s.expID}
	err := s.db.QueryRowContext(ctx, `SELECT r.generation,
 CASE WHEN d.generation=r.generation THEN d.acknowledged_revision ELSE 0 END,
 r.content_rev,(`+graphRevisionSQL+`),
 CASE WHEN d.generation=r.generation THEN d.graph_revision ELSE '' END
 FROM graph_revisions r LEFT JOIN planner_delivery_state d ON d.exploration_id=r.exploration_id
 WHERE r.exploration_id=$1`, s.expID, false).Scan(&out.Generation, &out.Lower, &out.Upper, &out.GraphRevision, &out.PreviousRevision)
	return out, err
}

// Acknowledge only what this round observed. CAS protects against older rounds
// overwriting newer delivery state. The task graph lock serializes this short
// update with graph mutations; no traversal or model call runs under that lock.
func (s *ExplorationStore) AcknowledgePlannerEvents(ctx context.Context, snap PlannerEventSnapshot) error {
	if snap.ExplorationID != s.expID || snap.Generation <= 0 || snap.Lower < 0 || snap.Upper < snap.Lower || snap.GraphRevision == "" {
		return fmt.Errorf("invalid planner event snapshot")
	}
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Parent deletion can hold this row before entering its graph trigger. Take
	// the parent lock first and never wait on it while holding the graph lock.
	var parent int64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM explorations WHERE id=$1 FOR KEY SHARE NOWAIT`, s.expID).Scan(&parent); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, s.expID+8100000000); err != nil {
		return err
	}
	var generation, current int64
	if err = tx.QueryRowContext(ctx, `SELECT generation,content_rev FROM graph_revisions WHERE exploration_id=$1`, s.expID).Scan(&generation, &current); err != nil {
		return err
	}
	if generation != snap.Generation || current < snap.Upper {
		return ErrPlannerWatermarkConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO planner_delivery_state(exploration_id,generation)
 VALUES($1,$2) ON CONFLICT(exploration_id) DO UPDATE SET generation=EXCLUDED.generation,
 acknowledged_revision=0,graph_revision='' WHERE planner_delivery_state.generation<>EXCLUDED.generation`, s.expID, snap.Generation); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE planner_delivery_state SET acknowledged_revision=$2,graph_revision=$3,updated_at=now()
 WHERE exploration_id=$1 AND generation=$4 AND ((acknowledged_revision=$5 AND graph_revision=$6)
 OR (acknowledged_revision=$2 AND graph_revision=$3))`, s.expID, snap.Upper, snap.GraphRevision, snap.Generation, snap.Lower, snap.PreviousRevision)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrPlannerWatermarkConflict
	}
	return tx.Commit()
}

// Only acknowledged derived metadata can be pruned; bound each deletion so a
// long task's first successful round never performs an unbounded cleanup.
func (s *ExplorationStore) PrunePlannerEvents(ctx context.Context, limit int) (int64, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `WITH page AS (
 SELECT l.generation,l.revision,l.node_id FROM planner_change_log l
 LEFT JOIN planner_delivery_state d USING(exploration_id)
 JOIN graph_revisions r USING(exploration_id)
 WHERE l.exploration_id=$1 AND (l.generation<>r.generation OR (d.generation=r.generation AND l.revision<=d.acknowledged_revision))
 ORDER BY l.revision,l.node_id LIMIT $2
 ) DELETE FROM planner_change_log l USING page p
 WHERE l.exploration_id=$1 AND l.generation=p.generation AND l.revision=p.revision AND l.node_id=p.node_id`, s.expID, min(1024, max(1, limit)))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type PlannerChange struct {
	ID       int64  `json:"node_id"`
	Ref      string `json:"ref"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Summary  string `json:"summary"`
	Revision int64  `json:"revision"`
}

// Filter by the snapshot boundary, node kind and live lineage authorization
// before pagination. No event payload or deleted/unauthorized body is returned.
func (s *ExplorationStore) PlannerChangePage(ctx context.Context, snap PlannerEventSnapshot, kinds []string, before int64, limit int) ([]PlannerChange, error) {
	if snap.ExplorationID != s.expID || snap.Generation <= 0 || snap.Lower < 0 || snap.Upper < snap.Lower || before < 0 || limit < 1 || limit > 64 {
		return nil, fmt.Errorf("invalid planner change page")
	}
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	if kinds == nil {
		kinds = []string{}
	}
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner
 FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.exploration_id=$1 AND n.kind=ANY($6::text[]) AND ($4::bigint=0 OR n.id<$4)
 AND EXISTS(SELECT 1 FROM planner_change_log l WHERE l.exploration_id=$1 AND l.generation=$8 AND l.node_id=n.id AND l.revision>$2 AND l.revision<=$3)
 AND EXISTS(SELECT 1 FROM graph_revisions r WHERE r.exploration_id=$1 AND r.generation=$8)`
	// Like PlannerBootstrap, the recursive authorization estimate can trigger
	// costly JIT compilation on an initial backlog, even though only a small
	// metadata page is returned. Scope the setting to this read transaction.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL jit=off`); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, contextVisibility(roots, false)+`
 SELECT n.id,'node:'||n.id,n.kind,n.state,left(COALESCE(n.payload->>'summary',n.payload->>'text',n.payload->>'body',''),240),
 (SELECT max(l.revision) FROM planner_change_log l WHERE l.exploration_id=$1 AND l.generation=$8 AND l.node_id=n.id AND l.revision>$2 AND l.revision<=$3)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id ORDER BY n.id DESC LIMIT $7`, s.expID, snap.Lower, snap.Upper, before, int64(0), kinds, limit+1, snap.Generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlannerChange{}
	for rows.Next() {
		var row PlannerChange
		if err = rows.Scan(&row.ID, &row.Ref, &row.Kind, &row.State, &row.Summary, &row.Revision); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}
