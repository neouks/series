package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Delete/rederive the ancestor region affected by topology changes. Regions
// include one-hop children of intent seeds, but do not expand child-only heat.
// This avoids root-count reference cycles and root x ancestor storage.
func maintainHotReachPage(ctx context.Context, tx *sql.Tx, exp int64) (bool, error) {
	var mode, phase int
	var cursor, wave, cutoff int64
	if err := tx.QueryRowContext(ctx, `SELECT hot_local_mode,hot_local_phase,hot_local_cursor,hot_build_generation,hot_local_cutoff FROM graph_revisions WHERE exploration_id=$1`, exp).Scan(&mode, &phase, &cursor, &wave, &cutoff); err != nil {
		return false, err
	}
	if mode < 2 {
		if mode == 0 {
			// Snapshot the queue before seeding. New mutations refresh their
			// ticket and survive bounded retirement of this baseline's events.
			if _, err := tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_local_mode=1,hot_local_cutoff=COALESCE((SELECT max(ticket) FROM graph_hot_changes WHERE exploration_id=$1),0),hot_ready=CASE WHEN hot_build_generation<>0 AND hot_wave<>topology_rev THEN true ELSE hot_ready END WHERE exploration_id=$1`, exp); err != nil {
				return false, err
			}
		}
		ready, err := maintainFullHotReachPage(ctx, tx, exp)
		if err != nil {
			return false, err
		}
		// A baseline completed at an older revision is still useful. Replay all
		// queued changes rather than restarting the whole graph under churn.
		if !ready {
			if err = tx.QueryRowContext(ctx, `SELECT hot_ready FROM graph_revisions WHERE exploration_id=$1`, exp).Scan(&ready); err != nil {
				return false, err
			}
		}
		if ready {
			_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_local_mode=2,hot_local_phase=7,hot_ready=false WHERE exploration_id=$1`, exp)
		}
		return false, err
	}

	next := func(p int) (bool, error) {
		_, err := tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_local_phase=$2,hot_local_cursor=0 WHERE exploration_id=$1`, exp, p)
		return false, err
	}
	if phase == 7 {
		var count int
		err := tx.QueryRowContext(ctx, `WITH page AS(SELECT node_id FROM graph_hot_changes WHERE exploration_id=$1 AND ticket<=$2 ORDER BY ticket,node_id LIMIT 128),removed AS(DELETE FROM graph_hot_changes c USING page p WHERE c.exploration_id=$1 AND c.node_id=p.node_id RETURNING 1) SELECT count(*) FROM removed`, exp, cutoff).Scan(&count)
		if err != nil {
			return false, err
		}
		if count == 0 {
			return next(0)
		}
		return false, nil
	}
	if phase == 0 {
		var count int
		err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED(SELECT node_id,children FROM graph_hot_changes WHERE exploration_id=$1 ORDER BY ticket,node_id LIMIT 128),removed AS(DELETE FROM graph_hot_changes c USING page p WHERE c.exploration_id=$1 AND c.node_id=p.node_id RETURNING c.node_id,c.children),written AS(INSERT INTO graph_hot_region(exploration_id,node_id,children) SELECT $1,node_id,children FROM removed ON CONFLICT(exploration_id,node_id) DO UPDATE SET children=graph_hot_region.children OR EXCLUDED.children RETURNING 1) SELECT count(*) FROM written`, exp).Scan(&count)
		if err != nil {
			return false, err
		}
		if count == 0 {
			_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_ready=true,hot_wave=topology_rev WHERE exploration_id=$1 AND (NOT hot_ready OR hot_wave<>topology_rev)`, exp)
			return err == nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_ready=false WHERE exploration_id=$1`, exp); err != nil {
			return false, err
		}
		return next(1)
	}
	if phase == 1 {
		return discoverHotRegion(ctx, tx, exp, next)
	}
	if phase == 4 {
		return propagateHotRegion(ctx, tx, exp, wave, next)
	}
	// Clear, seed, publish and cleanup each use an independent durable ID cursor.
	var ids []int64
	rows, err := tx.QueryContext(ctx, `SELECT node_id FROM graph_hot_region WHERE exploration_id=$1 AND node_id>$2 ORDER BY node_id LIMIT 128`, exp, cursor)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		if phase == 6 {
			return next(0)
		}
		return next(phase + 1)
	}
	switch phase {
	case 2:
		_, err = tx.ExecContext(ctx, `DELETE FROM graph_hot_reach WHERE exploration_id=$1 AND node_id=ANY($2::bigint[])`, exp, ids)
	case 3:
		_, err = tx.ExecContext(ctx, `WITH seeds AS MATERIALIZED(
 SELECT n.id,(n.kind='intent' AND n.state IN ('open','running','paused')) OR EXISTS(
 SELECT 1 FROM exploration_edges e JOIN graph_hot_reach h ON h.exploration_id=$1 AND h.node_id=e.dst_id AND h.wave=$3 AND h.expand
 WHERE e.exploration_id=$1 AND e.src_id=n.id AND e.rel<>'covers' AND NOT EXISTS(SELECT 1 FROM graph_hot_region r WHERE r.exploration_id=$1 AND r.node_id=e.dst_id)) expanding,
 EXISTS(SELECT 1 FROM exploration_edges e JOIN exploration_nodes p ON p.id=e.src_id AND p.exploration_id=$1 AND p.kind='intent' AND p.state IN ('open','running','paused') WHERE e.exploration_id=$1 AND e.dst_id=n.id AND e.rel<>'covers') child
 FROM exploration_nodes n WHERE n.exploration_id=$1 AND n.id=ANY($2::bigint[]))
 INSERT INTO graph_hot_reach(exploration_id,node_id,wave,expand,expanded,children_done)
 SELECT $1,id,$3,expanding,NOT expanding,true FROM seeds WHERE expanding OR child`, exp, ids, wave)
	case 5:
		_, err = tx.ExecContext(ctx, `UPDATE graph_node_projection p SET hot=EXISTS(SELECT 1 FROM graph_hot_reach h WHERE h.exploration_id=$1 AND h.node_id=p.node_id AND h.wave=$3),cold_since_at=CASE WHEN EXISTS(SELECT 1 FROM graph_hot_reach h WHERE h.exploration_id=$1 AND h.node_id=p.node_id AND h.wave=$3) THEN NULL ELSE COALESCE(p.cold_since_at,clock_timestamp()) END WHERE p.exploration_id=$1 AND p.node_id=ANY($2::bigint[])`, exp, ids, wave)
	case 6:
		_, err = tx.ExecContext(ctx, `DELETE FROM graph_hot_region WHERE exploration_id=$1 AND node_id=ANY($2::bigint[])`, exp, ids)
	}
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_local_cursor=$2 WHERE exploration_id=$1`, exp, ids[len(ids)-1])
	return false, err
}

func discoverHotRegion(ctx context.Context, tx *sql.Tx, exp int64, next func(int) (bool, error)) (bool, error) {
	type row struct {
		ID, Parent, Child int64
		Children          bool
		Parents, Kids     []int64
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED(SELECT * FROM graph_hot_region WHERE exploration_id=$1 AND NOT discovered ORDER BY node_id LIMIT 8)
 SELECT COALESCE(jsonb_agg(jsonb_build_object('ID',p.node_id,'Parent',p.parent_cursor,'Child',p.child_cursor,'Children',p.children,
 'Parents',(SELECT COALESCE(jsonb_agg(id),'[]') FROM(SELECT DISTINCT e.src_id id FROM exploration_edges e WHERE e.exploration_id=$1 AND e.dst_id=p.node_id AND e.rel<>'covers' AND e.src_id>p.parent_cursor ORDER BY id LIMIT 129)x),
 'Kids',(SELECT COALESCE(jsonb_agg(id),'[]') FROM(SELECT DISTINCT e.dst_id id FROM exploration_edges e WHERE p.children AND e.exploration_id=$1 AND e.src_id=p.node_id AND e.rel<>'covers' AND e.dst_id>p.child_cursor ORDER BY id LIMIT 129)x))),'[]') FROM page p`, exp).Scan(&raw)
	if err != nil {
		return false, err
	}
	var page []row
	if err = json.Unmarshal(raw, &page); err != nil {
		return false, err
	}
	if len(page) == 0 {
		return next(2)
	}
	for _, r := range page {
		moreP, moreC := len(r.Parents) > 128, len(r.Kids) > 128
		r.Parents = r.Parents[:min(128, len(r.Parents))]
		r.Kids = r.Kids[:min(128, len(r.Kids))]
		neighbors := append(append([]int64{}, r.Parents...), r.Kids...)
		if _, err = tx.ExecContext(ctx, `INSERT INTO graph_hot_region(exploration_id,node_id) SELECT $1,x FROM unnest($2::bigint[])x ON CONFLICT DO NOTHING`, exp, neighbors); err != nil {
			return false, err
		}
		if len(r.Parents) > 0 {
			r.Parent = r.Parents[len(r.Parents)-1]
		}
		if len(r.Kids) > 0 {
			r.Child = r.Kids[len(r.Kids)-1]
		}
		if _, err = tx.ExecContext(ctx, `UPDATE graph_hot_region SET parent_cursor=$3,child_cursor=$4,discovered=$5 WHERE exploration_id=$1 AND node_id=$2`, exp, r.ID, r.Parent, r.Child, !moreP && !moreC); err != nil {
			return false, err
		}
	}
	return false, nil
}
func propagateHotRegion(ctx context.Context, tx *sql.Tx, exp, wave int64, next func(int) (bool, error)) (bool, error) {
	type row struct {
		ID, Cursor int64
		Parents    []int64
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED(SELECT h.* FROM graph_hot_reach h JOIN graph_hot_region r ON r.exploration_id=h.exploration_id AND r.node_id=h.node_id WHERE h.exploration_id=$1 AND h.wave=$2 AND h.expand AND NOT h.expanded ORDER BY h.node_id LIMIT 8)
 SELECT COALESCE(jsonb_agg(jsonb_build_object('ID',p.node_id,'Cursor',p.edge_cursor,'Parents',(SELECT COALESCE(jsonb_agg(id),'[]') FROM(SELECT DISTINCT e.src_id id FROM exploration_edges e JOIN graph_hot_region r ON r.exploration_id=$1 AND r.node_id=e.src_id WHERE e.exploration_id=$1 AND e.dst_id=p.node_id AND e.rel<>'covers' AND e.src_id>p.edge_cursor ORDER BY id LIMIT 129)x))),'[]') FROM page p`, exp, wave).Scan(&raw)
	if err != nil {
		return false, err
	}
	var page []row
	if err = json.Unmarshal(raw, &page); err != nil {
		return false, err
	}
	if len(page) == 0 {
		return next(5)
	}
	for _, r := range page {
		more := len(r.Parents) > 128
		r.Parents = r.Parents[:min(128, len(r.Parents))]
		if _, err = tx.ExecContext(ctx, `INSERT INTO graph_hot_reach(exploration_id,node_id,wave,children_done) SELECT $1,x,$2,true FROM unnest($3::bigint[])x ON CONFLICT(exploration_id,node_id) DO UPDATE SET expand=true,expanded=false,edge_cursor=0 WHERE NOT graph_hot_reach.expand`, exp, wave, r.Parents); err != nil {
			return false, err
		}
		if len(r.Parents) > 0 {
			r.Cursor = r.Parents[len(r.Parents)-1]
		}
		if _, err = tx.ExecContext(ctx, `UPDATE graph_hot_reach SET edge_cursor=$3,expanded=$4 WHERE exploration_id=$1 AND node_id=$2`, exp, r.ID, r.Cursor, !more); err != nil {
			return false, err
		}
	}
	return false, nil
}
