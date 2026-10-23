package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type PlannerCount struct {
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Inherited bool   `json:"inherited"`
	Count     int    `json:"count"`
}

type PlannerContextRecord struct {
	Ref          string `json:"ref"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	SourceTaskID int64  `json:"source_task_id"`
	Inherited    bool   `json:"inherited,omitempty"`
	Summary      string `json:"summary"`
	Reason       string `json:"reason"`
}

type PlannerContext struct {
	Counts             []PlannerCount         `json:"counts"`
	Records            []PlannerContextRecord `json:"records"`
	HasMore            bool                   `json:"has_more"`
	TaskDescription    string                 `json:"task_description"`
	DescriptionOmitted bool                   `json:"description_omitted"`
}

// Planner only needs the ordering policy, not every queued intent payload or a
// queue mutation lock. Claim/reorder retain their existing transactional reads.
func (s *ExplorationStore) PlannerQueueManual(ctx context.Context) (bool, error) {
	var manual bool
	err := s.db.QueryRowContext(ctx, `SELECT worker_queue_manual FROM explorations WHERE id=$1`, s.expID).Scan(&manual)
	return manual, err
}

// PlannerBootstrap projects only counters and event-related summaries. Visibility
// precedes counts, relation expansion and LIMIT in one statement/snapshot. Bodies,
// graph edges and the full candidate set never leave PostgreSQL.
func (s *ExplorationStore) PlannerBootstrap(ctx context.Context, kinds []string, seeds []int64, heartbeat bool) (PlannerContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if kinds == nil {
		kinds = []string{}
	}
	if seeds == nil {
		seeds = []int64{}
	}
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner
 FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.kind=ANY($2::text[])
 AND (n.exploration_id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))
 AND (n.kind<>'digest' OR n.state='active')`
	query := contextVisibility(roots, false) + `, event_nodes AS MATERIALIZED (
 SELECT * FROM visible WHERE id=ANY($3::bigint[])
 ), candidates AS (
 SELECT id,0 rank FROM event_nodes
 UNION ALL SELECT v.id,1 FROM event_nodes s JOIN exploration_edges e ON e.dst_id=s.id AND e.exploration_id=s.exploration_id
 JOIN visible v ON v.id=e.src_id WHERE e.rel='derived_from'
 UNION ALL SELECT v.id,2 FROM event_nodes s JOIN exploration_edges e ON e.src_id=s.id AND e.exploration_id=s.exploration_id
 JOIN visible v ON v.id=e.dst_id WHERE e.rel='yields'
 UNION ALL SELECT v.id,3 FROM visible v WHERE v.kind='intent' AND v.state IN ('open','running')
 AND EXISTS(SELECT 1 FROM anchors a JOIN anchors b ON b.asset_id=a.asset_id JOIN event_nodes s ON s.id=b.root WHERE a.root=v.id)
 ), ranked AS (SELECT id,min(rank) rank FROM candidates GROUP BY id), idle AS (
 SELECT v.id,4 rank FROM visible v LEFT JOIN LATERAL (
 SELECT max(created_at) last_activity FROM activity WHERE exploration_id=v.exploration_id AND node_id=v.id
 ) a ON true WHERE $4::boolean AND v.kind='intent' AND v.state='running' AND v.exploration_id=$1
 ORDER BY COALESCE(a.last_activity,v.created_at),v.id ASC LIMIT 2
 ), chosen AS (
 SELECT * FROM (SELECT * FROM ranked UNION ALL SELECT * FROM idle WHERE NOT EXISTS(SELECT 1 FROM ranked)) c ORDER BY rank,id DESC LIMIT 9
 ), grouped AS (
 SELECT kind,CASE WHEN state IN ('open','running','paused','done','blocked','exhausted','stopped','met','active','confirmed','origin','abandoned','dismissed','consumed','superseded') THEN state ELSE 'other' END state,
 exploration_id<>$1 inherited,count(*) count FROM visible GROUP BY 1,2,3
 ) SELECT jsonb_build_object('task_description',(SELECT left(COALESCE(description,''),240) FROM explorations WHERE id=$1),
 'description_omitted',(SELECT length(COALESCE(description,''))>240 FROM explorations WHERE id=$1),
 'counts',COALESCE((SELECT jsonb_agg(to_jsonb(g) ORDER BY kind,state,inherited) FROM grouped g),'[]'::jsonb),
 'has_more',(SELECT count(*)>8 FROM chosen),'records',COALESCE((SELECT jsonb_agg(r ORDER BY rank,id DESC) FROM (
 SELECT 'node:'||v.id ref,v.kind,v.state,v.owner source_task_id,v.exploration_id<>$1 inherited,
 left(COALESCE(n.payload->>'summary',n.payload->>'text',n.payload->>'body',''),240) summary,
 CASE c.rank WHEN 0 THEN 'event' WHEN 1 THEN 'parent' WHEN 2 THEN 'yield' WHEN 3 THEN 'same_asset_active' ELSE 'idle_running' END reason,c.rank,v.id
 FROM chosen c JOIN visible v ON v.id=c.id JOIN exploration_nodes n ON n.id=v.id ORDER BY c.rank,v.id DESC LIMIT 8
 ) r),'[]'::jsonb))`
	var out PlannerContext
	// The recursive visibility plan's conservative row estimates trigger LLVM
	// JIT even for a small graph. EXPLAIN measured >800ms compiling this bounded
	// projection. Keep JIT disabled for this read transaction only; do not leak a
	// session setting into the shared connection pool.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL jit=off`); err != nil {
		return out, err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, query, s.expID, kinds, seeds, heartbeat, int64(0)).Scan(&raw)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
