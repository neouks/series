package db

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Called with the graph advisory lock held. Roots and each edge list have their
// own durable cursors, so deep chains, cycles and high fanout can resume across
// jobs/restarts. Only IDs and lifecycle state are inspected, never payloads.
func maintainHotReachPage(ctx context.Context, tx *sql.Tx, exp int64) (bool, error) {
	var topology, wave, built, cursor int64
	var seeded, ready bool
	if err := tx.QueryRowContext(ctx, `SELECT topology_rev,hot_build_generation,hot_wave,hot_seed_cursor,hot_seed_done,hot_ready FROM graph_revisions WHERE exploration_id=$1`, exp).Scan(&topology, &wave, &built, &cursor, &seeded, &ready); err != nil {
		return false, err
	}
	if built != topology {
		if _, err := tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_wave=topology_rev,hot_build_generation=nextval('graph_projection_generation'),hot_seed_cursor=0,hot_seed_done=false,hot_ready=false WHERE exploration_id=$1`, exp); err != nil {
			return false, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT hot_build_generation FROM graph_revisions WHERE exploration_id=$1`, exp).Scan(&wave); err != nil {
			return false, err
		}
		cursor, seeded, ready = 0, false, false
	}
	if ready {
		return true, nil
	}
	if !seeded {
		var last int64
		var count int
		err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED (
   SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running','paused') AND id>$2 ORDER BY id LIMIT 128
  ), written AS (
   INSERT INTO graph_hot_reach(exploration_id,node_id,wave) SELECT $1::bigint,id,$3::bigint FROM page
   ON CONFLICT(exploration_id,node_id) DO UPDATE SET wave=EXCLUDED.wave,expand=true,expanded=false,edge_cursor=0,child_cursor=0,children_done=false
   WHERE graph_hot_reach.wave<>EXCLUDED.wave OR NOT graph_hot_reach.expand RETURNING node_id
  ) SELECT COALESCE(max(id),$2),count(*) FROM page`, exp, cursor, wave).Scan(&last, &count)
		if err != nil {
			return false, err
		}
		seeded = count < 128
		if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_seed_cursor=$2,hot_seed_done=$3 WHERE exploration_id=$1`, exp, last, seeded); err != nil {
			return false, err
		}
		return false, nil
	}
	type frontier struct {
		ID           int64   `json:"id"`
		Ancestors    []int64 `json:"ancestors"`
		Children     []int64 `json:"children"`
		Edge         int64   `json:"edge"`
		Child        int64   `json:"child"`
		ChildrenDone bool    `json:"children_done"`
	}
	// Read at most 128 frontier records and 129 neighbors in either direction
	// per record; the 129th only signals a further page. Cursors use node IDs,
	// with DISTINCT so multiple edge relations cannot drop a neighbor.
	var raw []byte
	err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED (
 SELECT h.*,n.kind='intent' AND n.state IN ('open','running','paused') live FROM graph_hot_reach h
 LEFT JOIN exploration_nodes n ON n.id=h.node_id WHERE h.exploration_id=$1 AND h.wave=$2 AND NOT h.expanded ORDER BY h.node_id LIMIT 128
 ) SELECT COALESCE(jsonb_agg(jsonb_build_object('id',p.node_id,'edge',p.edge_cursor,'child',p.child_cursor,'children_done',p.children_done,
 'ancestors',(SELECT COALESCE(jsonb_agg(id ORDER BY id),'[]'::jsonb) FROM (SELECT DISTINCT e.src_id id FROM exploration_edges e WHERE p.expand AND e.exploration_id=$1 AND e.dst_id=p.node_id AND e.rel<>'covers' AND e.src_id>p.edge_cursor ORDER BY e.src_id LIMIT 129)a),
 'children',(SELECT COALESCE(jsonb_agg(id ORDER BY id),'[]'::jsonb) FROM (SELECT DISTINCT e.dst_id id FROM exploration_edges e WHERE p.live AND NOT p.children_done AND e.exploration_id=$1 AND e.src_id=p.node_id AND e.rel<>'covers' AND e.dst_id>p.child_cursor ORDER BY e.dst_id LIMIT 129)b)
 )),'[]'::jsonb) FROM page p`, exp, wave).Scan(&raw)
	if err != nil {
		return false, err
	}
	var page []frontier
	if err = json.Unmarshal(raw, &page); err != nil {
		return false, err
	}
	if len(page) == 0 {
		_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET hot_ready=true WHERE exploration_id=$1 AND hot_wave=topology_rev`, exp)
		return err == nil, err
	}
	ancestors, children := []int64{}, []int64{}
	updates := []map[string]any{}
	for _, p := range page {
		moreParents, moreChildren := len(p.Ancestors) > 128, len(p.Children) > 128
		p.Ancestors = p.Ancestors[:min(128, len(p.Ancestors))]
		p.Children = p.Children[:min(128, len(p.Children))]
		ancestors = append(ancestors, p.Ancestors...)
		children = append(children, p.Children...)
		if len(p.Ancestors) > 0 {
			p.Edge = p.Ancestors[len(p.Ancestors)-1]
		}
		if len(p.Children) > 0 {
			p.Child = p.Children[len(p.Children)-1]
		}
		updates = append(updates, map[string]any{"id": p.ID, "edge": p.Edge, "child": p.Child, "children_done": !moreChildren, "done": !moreParents && !moreChildren})
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO graph_hot_reach(exploration_id,node_id,wave)
 SELECT DISTINCT $1::bigint,x,$2::bigint FROM unnest($3::bigint[])x
 ON CONFLICT(exploration_id,node_id) DO UPDATE SET wave=EXCLUDED.wave,expand=true,expanded=false,edge_cursor=0,child_cursor=0,children_done=false
 WHERE graph_hot_reach.wave<>EXCLUDED.wave OR NOT graph_hot_reach.expand`, exp, wave, ancestors); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO graph_hot_reach(exploration_id,node_id,wave,expand,expanded,children_done)
 SELECT DISTINCT $1::bigint,x,$2::bigint,false,true,true FROM unnest($3::bigint[])x
 ON CONFLICT(exploration_id,node_id) DO UPDATE SET wave=EXCLUDED.wave,expand=false,expanded=true,edge_cursor=0,child_cursor=0,children_done=true
 WHERE graph_hot_reach.wave<>EXCLUDED.wave`, exp, wave, children); err != nil {
		return false, err
	}
	progress, _ := json.Marshal(updates)
	_, err = tx.ExecContext(ctx, `UPDATE graph_hot_reach h SET edge_cursor=p.edge,child_cursor=p.child,children_done=p.children_done,expanded=p.done
 FROM jsonb_to_recordset($3::jsonb) p(id bigint,edge bigint,child bigint,children_done boolean,done boolean)
 WHERE h.exploration_id=$1 AND h.wave=$2 AND h.node_id=p.id`, exp, wave, string(progress))
	return false, err
}
