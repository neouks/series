package db

import (
	"context"
	"time"
)

// The final transactional admission path must see its own uncommitted changes.
// Only standalone history/preflight reads use the locally tuned read-only tx.
func (s *AssetStore) nodeAccessQuery(query string, args ...any) (*contextRows, error) {
	ctx := s.readCtx
	if ctx == nil {
		ctx = context.Background()
	}
	query = workerReadSQL(query, s.workerRead)
	if s.tx != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var jit string
		if err := s.tx.QueryRowContext(ctx, `SELECT current_setting('jit')`).Scan(&jit); err != nil {
			cancel()
			return nil, err
		}
		var restore func() error
		if jit != "off" {
			if _, err := s.tx.ExecContext(ctx, `SET LOCAL jit=off`); err != nil {
				cancel()
				return nil, err
			}
			restore = func() error { _, err := s.tx.ExecContext(ctx, `SELECT set_config('jit',$1,true)`, jit); return err }
		}
		rows, err := s.tx.QueryContext(ctx, query, args...)
		if err != nil {
			if restore != nil {
				_ = restore()
			}
			cancel()
			return nil, err
		}
		return &contextRows{Rows: rows, cancel: cancel, afterClose: restore}, nil
	}
	return (&ExplorationStore{db: s.db}).contextQuery(ctx, query, args...)
}

// Denial reasons, unlike permission receipts, can be propagated once per
// (node,reason). Preserve the source/current task checks and public result shape
// without enumerating every candidate's complete ancestry and asset matrix.
const taskNodeAccessSQL = `WITH RECURSIVE current_task AS (
 SELECT id,exploration_id FROM tasks WHERE id=$1 AND deleted_at IS NULL
 ), contexts AS (
 SELECT exploration_id exp,id owner FROM current_task
 UNION SELECT t.exploration_id,t.id FROM task_relations relation JOIN current_task c ON c.id=relation.task_id
 JOIN tasks t ON t.id=relation.source_task_id AND t.deleted_at IS NULL
 ), roots AS MATERIALIZED (
 SELECT n.id,n.exploration_id,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id WHERE n.id=ANY($2::bigint[])
 ), reachable(id,exp) AS (
 SELECT id,exploration_id FROM roots
 UNION SELECT step.id,r.exp FROM reachable r CROSS JOIN LATERAL (` + contextDependencySteps + `)step
 ), dependencies AS MATERIALIZED (
 SELECT r.id node_id,step.id dependency_id,r.exp FROM reachable r CROSS JOIN LATERAL (` + contextDependencySteps + `)step
 ), direct_assets AS MATERIALIZED (
 SELECT r.id node_id,r.exp,a.asset_id FROM reachable r JOIN exploration_nodes source_node ON source_node.id=r.id AND source_node.exploration_id=r.exp
 JOIN exploration_anchors a ON a.node_id=r.id
 UNION SELECT r.id,r.exp,CASE WHEN x.value ~ '^[0-9]{1,18}$' THEN x.value::bigint ELSE 0 END
 FROM reachable r JOIN exploration_nodes n ON n.id=r.id AND n.exploration_id=r.exp
 LEFT JOIN graph_node_projection gp ON gp.node_id=n.id AND gp.asset_metadata_ready
 CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN gp.node_id IS NOT NULL THEN to_jsonb(gp.payload_asset_ids)
 ELSE CASE WHEN jsonb_typeof(n.payload->'asset_ids')='array' THEN n.payload->'asset_ids' ELSE '[]'::jsonb END END)x(value)
 ), permission_keys AS MATERIALIZED (
 SELECT DISTINCT c.owner,a.asset_id FROM direct_assets a JOIN contexts c ON c.exp=a.exp
 ), permissions AS MATERIALIZED (
 SELECT owner,asset_id,task_asset_effectively_approved($1,asset_id) AND task_asset_effectively_approved(owner,asset_id) allowed,
 task_asset_effective_approval_state($1,asset_id) current_state,task_asset_effective_approval_state(owner,asset_id) owner_state FROM permission_keys
 ), denials(node_id,exp,reason) AS (
 SELECT r.id,r.exp,'unavailable'::text FROM reachable r LEFT JOIN exploration_nodes n ON n.id=r.id AND n.exploration_id=r.exp
 WHERE n.id IS NULL OR (n.kind='digest' AND (NOT series_digest_sources_current(n.payload)
 OR (NOT $3::boolean AND NOT EXISTS(SELECT 1 FROM exploration_edges e WHERE e.exploration_id=n.exploration_id AND e.src_id=n.id AND e.rel='covers'))))
 UNION SELECT a.node_id,a.exp,CASE WHEN NOT EXISTS(SELECT 1 FROM assets asset WHERE asset.id=a.asset_id) THEN 'unavailable' ELSE st.reason END
 FROM direct_assets a JOIN contexts c ON c.exp=a.exp JOIN permissions p ON p.owner=c.owner AND p.asset_id=a.asset_id
 CROSS JOIN LATERAL (VALUES(p.current_state),(p.owner_state))st(reason) WHERE NOT p.allowed AND st.reason<>'approved'
 UNION SELECT d.node_id,d.exp,bad.reason FROM denials bad JOIN dependencies d ON d.dependency_id=bad.node_id AND d.exp=bad.exp
 ) SELECT r.id,COALESCE((SELECT jsonb_agg(DISTINCT reason ORDER BY reason) FROM denials bad
 WHERE bad.node_id=r.id AND bad.exp=r.exploration_id),'[]'::jsonb) FROM roots r`
