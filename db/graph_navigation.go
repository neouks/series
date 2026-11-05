package db

import (
	"context"
	"database/sql"
)

// Callers must still scope and authorize n using the original business reads.
// The cache never grants access. The fallback is the same bounded field prefix,
// computed from the live payload when a row/rule/physical layout is stale.
const nodeNavigationJoinSQL = ` LEFT JOIN graph_navigation_projection np ON np.node_id=n.id AND np.exploration_id=n.exploration_id `
const nodeNavigationCurrentSQL = `np.rule_version=series_graph_navigation_rule() AND np.source_stamp=n.xmin::text||':'||n.ctid::text`
const nodeNavigationPayloadSQL = `(CASE WHEN ` + nodeNavigationCurrentSQL + ` THEN np.preview ELSE n.payload END)`
const nodeNavigationDisplaySQL = `COALESCE(NULLIF(` + nodeNavigationPayloadSQL + `->>'name',''),NULLIF(` + nodeNavigationPayloadSQL + `->>'summary',''),
 NULLIF(` + nodeNavigationPayloadSQL + `->>'text',''),NULLIF(` + nodeNavigationPayloadSQL + `->>'description',''),NULLIF(` + nodeNavigationPayloadSQL + `->>'body',''),'')`
const nodeNavigationContextSQL = `COALESCE(` + nodeNavigationPayloadSQL + `->>'summary',` + nodeNavigationPayloadSQL + `->>'text',` + nodeNavigationPayloadSQL + `->>'body','')`

// Metadata scans are 128 ids; at most eight stale payloads are rebuilt per
// transaction. The cursor never advances past an unprocessed stale row.
func maintainGraphNavigationPage(ctx context.Context, tx *sql.Tx, expID int64) (bool, error) {
	var cursor int64
	var rule int
	var relation, currentRelation int64
	var ready bool
	var currentRule int
	err := tx.QueryRowContext(ctx, `SELECT navigation_cursor,navigation_rule,navigation_relation::bigint,navigation_ready,
 pg_relation_filenode('exploration_nodes'::regclass)::bigint,series_graph_navigation_rule() FROM graph_revisions WHERE exploration_id=$1`, expID).Scan(&cursor, &rule, &relation, &ready, &currentRelation, &currentRule)
	if err != nil {
		return false, err
	}
	if rule != currentRule || relation != currentRelation {
		cursor, ready = 0, false
	}
	if ready {
		return true, nil
	}
	var last int64
	err = tx.QueryRowContext(ctx, `WITH ids AS MATERIALIZED (
 SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND id>$2 ORDER BY id LIMIT 128
 ), metadata AS MATERIALIZED (
 SELECT n.id,n.xmin::text||':'||n.ctid::text stamp,p.source_stamp base_stamp,p.rule_version base_rule,
 p.exploration_id=n.exploration_id AND p.rule_version=$3 AND p.source_stamp=n.xmin::text||':'||n.ctid::text AS valid
 FROM ids i JOIN exploration_nodes n ON n.id=i.id LEFT JOIN graph_navigation_projection p ON p.node_id=n.id
 ), ranked AS MATERIALIZED (
 SELECT *,sum(CASE WHEN valid THEN 0 ELSE 1 END) OVER(ORDER BY id) stale FROM metadata
 ), eligible AS MATERIALIZED (SELECT * FROM ranked WHERE stale<=8), prepared AS MATERIALIZED (
 SELECT e.*,series_graph_navigation_preview(n.payload) preview FROM eligible e JOIN exploration_nodes n ON n.id=e.id WHERE e.valid IS NOT TRUE
 ), inserted AS (
 INSERT INTO graph_navigation_projection(node_id,exploration_id,source_stamp,rule_version,preview)
 SELECT id,$1,stamp,$3,preview FROM prepared
 ON CONFLICT(node_id) DO UPDATE SET exploration_id=EXCLUDED.exploration_id,source_stamp=EXCLUDED.source_stamp,
 rule_version=EXCLUDED.rule_version,preview=EXCLUDED.preview
 WHERE (graph_navigation_projection.source_stamp,graph_navigation_projection.rule_version) IS NOT DISTINCT FROM
 (SELECT ROW(base_stamp,base_rule) FROM prepared p WHERE p.id=EXCLUDED.node_id)
 RETURNING node_id
 ) SELECT COALESCE(max(id),0) FROM eligible`, expID, cursor, currentRule).Scan(&last)
	if err != nil {
		return false, err
	}
	ready = last == 0
	if last > 0 {
		cursor = last
	}
	_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET navigation_cursor=$2,navigation_rule=$3,navigation_relation=$4::oid,navigation_ready=$5 WHERE exploration_id=$1`, expID, cursor, currentRule, currentRelation, ready)
	return ready, err
}

// Explicit metadata rebuild. Raw nodes, graph revisions, audit and model state
// are untouched; valid cache rows stay usable while bounded pages catch up.
func (s *ExplorationStore) ResetGraphNavigationProjection(ctx context.Context) error {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, s.expID+8100000000); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET navigation_cursor=0,navigation_ready=false WHERE exploration_id=$1`, s.expID); err != nil {
		return err
	}
	return tx.Commit()
}
