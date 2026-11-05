package db

import (
	"context"
	"database/sql"
)

// All projection writes follow the graph advisory lock. Pages have independent
// edge and JSON-array cursors; neither a large fanout nor a malformed legacy
// digest requires loading its complete body or dependency set into Go.
func maintainDependencyProjectionPage(ctx context.Context, tx *sql.Tx, exp int64) (int, error) {
	var generation, cursor, workCursor int64
	var initialized bool
	if err := tx.QueryRowContext(ctx, `SELECT dependency_generation,dependency_cursor,dependency_initialized,dependency_work_cursor FROM graph_revisions WHERE exploration_id=$1`, exp).Scan(&generation, &cursor, &initialized, &workCursor); err != nil {
		return 0, err
	}
	work := 0
	if !initialized {
		var last int64
		var count int
		err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED (
 SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND id>$2 ORDER BY id LIMIT 128
 ), inserted AS (
 INSERT INTO graph_dependency_state(node_id,exploration_id,generation) SELECT id,$1::bigint,$3::bigint FROM page
 ON CONFLICT(node_id) DO UPDATE SET exploration_id=EXCLUDED.exploration_id,generation=EXCLUDED.generation,
 revision=graph_dependency_state.revision+1,work=true,rule_version=1
 WHERE graph_dependency_state.generation<>EXCLUDED.generation OR graph_dependency_state.rule_version<>1
 ) SELECT COALESCE(max(id),$2),count(*) FROM page`, exp, cursor, generation).Scan(&last, &count)
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET dependency_cursor=$2,dependency_initialized=$3 WHERE exploration_id=$1`, exp, last, count < 128); err != nil {
			return 0, err
		}
		work = 1
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id FROM graph_dependency_state WHERE exploration_id=$1 AND work AND node_id>$2 ORDER BY node_id LIMIT 8 FOR UPDATE`, exp, workCursor)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		if workCursor > 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET dependency_work_cursor=0 WHERE exploration_id=$1`, exp); err != nil {
				return 0, err
			}
			var pending bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM graph_dependency_state WHERE exploration_id=$1 AND work)`, exp).Scan(&pending); err != nil {
				return 0, err
			}
			if pending {
				work = 1
			}
		}
		return work, nil
	}
	for _, id := range ids {
		if err = maintainDependencyNode(ctx, tx, exp, id, generation); err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET dependency_work_cursor=$2 WHERE exploration_id=$1`, exp, ids[len(ids)-1])
	return work + len(ids), err
}

func maintainDependencyNode(ctx context.Context, tx *sql.Tx, exp, id, generation int64) error {
	// An interrupted/changed build keeps its old rows, ignored by readers. The
	// primary key reuses shared dependencies instead of storing every generation.
	if _, err := tx.ExecContext(ctx, `UPDATE graph_dependency_state SET building_revision=revision,projected_revision=0,building_count=0,
 generation=$3,edge_cursor=0,anchor_cursor=0,edges_done=false,anchors_done=false,rule_version=1
 WHERE node_id=$2 AND exploration_id=$1 AND (building_revision<>revision OR generation<>$3 OR rule_version<>1)`, exp, id, generation); err != nil {
		return err
	}
	var revision, edge, buildingCount int64
	var anchor int
	var edgesDone, anchorsDone bool
	if err := tx.QueryRowContext(ctx, `SELECT revision,edge_cursor,anchor_cursor,edges_done,anchors_done,building_count FROM graph_dependency_state WHERE node_id=$1`, id).Scan(&revision, &edge, &anchor, &edgesDone, &anchorsDone, &buildingCount); err != nil {
		return err
	}
	if !edgesDone {
		var last int64
		var count int
		var written int64
		err := tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED (
 SELECT id FROM (
  (SELECT DISTINCT e.dst_id id FROM exploration_edges e WHERE e.exploration_id=$1 AND e.src_id=$2 AND e.rel='covers' AND e.dst_id>$3 ORDER BY id LIMIT 128)
  UNION
  (SELECT DISTINCT e.src_id id FROM exploration_edges e WHERE e.exploration_id=$1 AND e.dst_id=$2 AND e.rel<>'covers' AND e.src_id>$3 ORDER BY id LIMIT 128)
 )neighbors ORDER BY id LIMIT 128
 ), written AS (
 INSERT INTO graph_dependencies(node_id,dependency_id,generation,revision) SELECT $2::bigint,id,$4::bigint,$5::bigint FROM page
 ON CONFLICT(node_id,dependency_id) DO UPDATE SET generation=EXCLUDED.generation,revision=EXCLUDED.revision
 WHERE (graph_dependencies.generation,graph_dependencies.revision) IS DISTINCT FROM (EXCLUDED.generation,EXCLUDED.revision) RETURNING 1
 ) SELECT COALESCE(max(id),$3),count(*),(SELECT count(*) FROM written) FROM page`, exp, id, edge, generation, revision).Scan(&last, &count, &written)
		if err != nil {
			return err
		}
		edge = last
		buildingCount += written
		edgesDone = count < 128
	}
	if !anchorsDone {
		var count int
		var written int64
		err := tx.QueryRowContext(ctx, `WITH source AS MATERIALIZED (
 SELECT CASE WHEN kind='digest' AND jsonb_typeof(payload->'anchor_ids')='array' THEN payload->'anchor_ids' ELSE '[]'::jsonb END anchors
 FROM exploration_nodes WHERE id=$1 AND exploration_id=$2
 ), page AS MATERIALIZED (
 SELECT CASE WHEN s.anchors->>i ~ '^[0-9]{1,18}$' THEN (s.anchors->>i)::bigint ELSE 0 END id
 FROM source s CROSS JOIN LATERAL generate_series($3,least(jsonb_array_length(s.anchors)-1,$3+127))i
 ), written AS (
 INSERT INTO graph_dependencies(node_id,dependency_id,generation,revision) SELECT DISTINCT $1::bigint,id,$4::bigint,$5::bigint FROM page
 ON CONFLICT(node_id,dependency_id) DO UPDATE SET generation=EXCLUDED.generation,revision=EXCLUDED.revision
 WHERE (graph_dependencies.generation,graph_dependencies.revision) IS DISTINCT FROM (EXCLUDED.generation,EXCLUDED.revision) RETURNING 1
 ) SELECT count(*),(SELECT count(*) FROM written) FROM page`, id, exp, anchor, generation, revision).Scan(&count, &written)
		if err != nil {
			return err
		}
		anchor += count
		buildingCount += written
		anchorsDone = count < 128
	}
	if _, err := tx.ExecContext(ctx, `UPDATE graph_dependency_state SET edge_cursor=$2,anchor_cursor=$3,edges_done=$4,anchors_done=$5,
	 projected_revision=CASE WHEN $4 AND $5 THEN revision ELSE projected_revision END,
 building_count=$6,projected_count=CASE WHEN $4 AND $5 THEN $6 ELSE projected_count END WHERE node_id=$1`, id, edge, anchor, edgesDone, anchorsDone, buildingCount); err != nil {
		return err
	}
	if !edgesDone || !anchorsDone {
		return nil
	}
	// Reclaim obsolete adjacency in bounded chunks, including removals and reset
	// generations. Cached reads only use rows of the published exact revision.
	if _, err := tx.ExecContext(ctx, `DELETE FROM graph_dependencies WHERE node_id=$1 AND dependency_id IN (
 SELECT dependency_id FROM graph_dependencies WHERE node_id=$1 AND (generation<>$2 OR revision<>$3) ORDER BY dependency_id LIMIT 128
 )`, id, generation, revision); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE graph_dependency_state SET work=EXISTS(SELECT 1 FROM graph_dependencies WHERE node_id=$1 AND (generation<>$2 OR revision<>$3)) WHERE node_id=$1`, id, generation, revision)
	return err
}

// Rebuild derived metadata without changing business revisions, facts or audit.
// Readers immediately fall back to live adjacency until each new generation is
// complete; background pages rediscover nodes and reclaim stale rows gradually.
func (s *ExplorationStore) ResetGraphDependencyProjection(ctx context.Context) error {
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
	if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET dependency_generation=nextval('graph_projection_generation'),dependency_cursor=0,dependency_work_cursor=0,dependency_initialized=false WHERE exploration_id=$1`, s.expID); err != nil {
		return err
	}
	return tx.Commit()
}
