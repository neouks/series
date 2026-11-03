package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const graphRevisionSQL = `WITH scope AS (
 SELECT $1::bigint id UNION SELECT t.exploration_id FROM tasks c JOIN task_relations r ON r.task_id=c.id
 JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL WHERE c.exploration_id=$1 AND c.deleted_at IS NULL
 ) SELECT COALESCE(string_agg(concat_ws(':',s.id,COALESCE(r.generation,0),COALESCE(r.content_rev,0),COALESCE(r.topology_rev,0),COALESCE(r.access_rev,0),
 CASE WHEN $2 THEN COALESCE(r.view_rev,0) ELSE 0 END),'|' ORDER BY s.id),'') FROM scope s LEFT JOIN graph_revisions r ON r.exploration_id=s.id`

// Revision vectors contain only directly related task epochs, never bodies.
func (s *ExplorationStore) GraphRevision(ctx context.Context, includeDerived bool) (string, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	var out string
	err := s.db.QueryRowContext(ctx, graphRevisionSQL, s.expID, includeDerived).Scan(&out)
	return out, err
}

// Bootstrap at most one page. Runtime writes already maintain their projection.
func (s *ExplorationStore) BootstrapGraphProjection(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, s.expID+8100000000); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO graph_revisions(exploration_id) SELECT id FROM explorations WHERE id=$1 ON CONFLICT DO NOTHING`, s.expID); err != nil {
		return false, err
	}
	var cursor int64
	var ready bool
	if err = tx.QueryRowContext(ctx, `SELECT bootstrap_cursor,initialized FROM graph_revisions WHERE exploration_id=$1 FOR UPDATE`, s.expID).Scan(&cursor, &ready); err != nil {
		return false, err
	}
	if ready {
		var missing bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM graph_node_projection WHERE exploration_id=$1 AND NOT asset_metadata_ready)`, s.expID).Scan(&missing); err != nil {
			return false, err
		}
		if !missing {
			return true, tx.Commit()
		}
		cursor = 0
	}
	var last int64
	err = tx.QueryRowContext(ctx, `WITH page AS MATERIALIZED (
 SELECT id,exploration_id,kind,state,payload FROM exploration_nodes WHERE exploration_id=$1 AND id>$2 ORDER BY id LIMIT 256
 ), inserted AS (
 INSERT INTO graph_node_projection(node_id,exploration_id,kind,state,summary,confidence,source_hash,payload_asset_ids,asset_metadata_ready)
 SELECT id,exploration_id,kind,state,left(COALESCE(payload->>'summary',payload->>'text',''),600),left(COALESCE(payload->>'confidence',''),40),md5(payload::text||state||kind),series_graph_payload_assets(payload),true
 FROM page WHERE kind<>'digest' ON CONFLICT(node_id) DO UPDATE SET payload_asset_ids=EXCLUDED.payload_asset_ids,asset_metadata_ready=true WHERE NOT graph_node_projection.asset_metadata_ready RETURNING node_id
 ), dirty AS (
 INSERT INTO graph_dirty_nodes(exploration_id,node_id) SELECT $1,node_id FROM inserted ON CONFLICT DO NOTHING
 ) SELECT COALESCE(max(id),0) FROM page`, s.expID, cursor).Scan(&last)
	if err != nil {
		return false, err
	}
	ready = last == 0
	_, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET bootstrap_cursor=greatest(bootstrap_cursor,$2),initialized=$3 WHERE exploration_id=$1`, s.expID, last, ready)
	if err != nil {
		return false, err
	}
	return ready, tx.Commit()
}

// Process a bounded frontier; propagation is persisted and therefore resumable
// through a deep/cyclic component without keeping the full graph in Go memory.
func (s *ExplorationStore) MaintainGraphPage(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, s.expID+8100000000); err != nil {
		return 0, err
	}
	ready, err := maintainHotReachPage(ctx, tx, s.expID)
	if err != nil {
		return 0, err
	}
	if !ready {
		return 1, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id,propagate FROM graph_dirty_nodes WHERE exploration_id=$1 ORDER BY node_id LIMIT 128 FOR UPDATE SKIP LOCKED`, s.expID)
	if err != nil {
		return 0, err
	}
	var ids, propagate []int64
	for rows.Next() {
		var id int64
		var prop bool
		if err = rows.Scan(&id, &prop); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		if prop {
			propagate = append(propagate, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit()
	}
	// Each revision wave is expanded once per node. A marker records the wave
	// in graph_repair_seen; real mutations clear it through the source revision.
	var wave int64
	if err = tx.QueryRowContext(ctx, `SELECT content_rev FROM graph_revisions WHERE exploration_id=$1`, s.expID).Scan(&wave); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `WITH affected AS (
 SELECT DISTINCT e.src_id id FROM exploration_edges e WHERE e.exploration_id=$1 AND e.dst_id=ANY($2::bigint[]) AND e.rel<>'covers'

 ), fresh AS (
 INSERT INTO graph_repair_seen(exploration_id,node_id,wave) SELECT $1,id,$3 FROM affected
 ON CONFLICT(exploration_id,node_id) DO UPDATE SET wave=EXCLUDED.wave WHERE graph_repair_seen.wave<>EXCLUDED.wave RETURNING node_id
 ) INSERT INTO graph_dirty_nodes(exploration_id,node_id) SELECT $1,node_id FROM fresh WHERE NOT(node_id=ANY($4::bigint[])) ON CONFLICT DO NOTHING`, s.expID, propagate, wave, ids)
	if err != nil {
		return 0, err
	}
	// A live intent's direct children need one recheck, not recursive expansion.
	if _, err = tx.ExecContext(ctx, `INSERT INTO graph_dirty_nodes(exploration_id,node_id,propagate)
 SELECT $1,e.dst_id,false FROM exploration_edges e WHERE e.exploration_id=$1 AND e.src_id=ANY($2::bigint[]) AND e.rel<>'covers'
 AND NOT(e.dst_id=ANY($3::bigint[])) ON CONFLICT DO NOTHING`, s.expID, propagate, ids); err != nil {
		return 0, err
	}
	// Reachability has already converged for this topology epoch. A lookup
	// replaces the old full active-ancestor traversal on every dirty page.
	_, err = tx.ExecContext(ctx, `UPDATE graph_node_projection p SET
 hot=EXISTS(SELECT 1 FROM graph_hot_reach h JOIN graph_revisions r ON r.exploration_id=h.exploration_id AND r.hot_build_generation=h.wave AND r.hot_wave=r.topology_rev WHERE h.exploration_id=$1 AND h.node_id=p.node_id),
 cold_since_at=CASE WHEN EXISTS(SELECT 1 FROM graph_hot_reach h JOIN graph_revisions r ON r.exploration_id=h.exploration_id AND r.hot_build_generation=h.wave AND r.hot_wave=r.topology_rev WHERE h.exploration_id=$1 AND h.node_id=p.node_id)
 THEN NULL ELSE COALESCE(p.cold_since_at,clock_timestamp()) END WHERE p.exploration_id=$1 AND p.node_id=ANY($2::bigint[])`, s.expID, ids)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM graph_dirty_nodes WHERE exploration_id=$1 AND node_id=ANY($2::bigint[])`, s.expID, ids)
	if err != nil {
		return 0, err
	}
	return len(ids), tx.Commit()
}

type GraphCandidate struct {
	ID         int64  `json:"node_id"`
	Version    int64  `json:"source_version"`
	Summary    string `json:"summary"`
	Confidence string `json:"confidence"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	GroupKey   string `json:"-"`
	Hash       string `json:"-"`
}

func (s *ExplorationStore) GraphCandidates(ctx context.Context, limit int) ([]GraphCandidate, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT p.node_id,p.source_version,p.summary,p.confidence,p.kind,p.state,
 COALESCE((SELECT string_agg(asset_id::text,',' ORDER BY asset_id) FROM exploration_anchors WHERE node_id=p.node_id),
 'parent:'||(SELECT min(src_id)::text FROM exploration_edges WHERE exploration_id=$1 AND dst_id=p.node_id AND rel<>'covers'), 'single:'||p.node_id),p.source_hash
 FROM graph_node_projection p
 WHERE p.exploration_id=$1 AND ((p.kind='fact' AND p.state='confirmed') OR (p.kind='intent' AND p.state IN ('done','blocked','exhausted','stopped'))) AND NOT p.hot AND p.cold_since_at<=now()-interval '10 minutes'
 AND p.node_id>(SELECT summary_cursor FROM graph_revisions WHERE exploration_id=$1)
 AND NOT EXISTS(SELECT 1 FROM graph_dirty_nodes d WHERE d.exploration_id=$1)
 AND EXISTS(SELECT 1 FROM graph_revisions r WHERE r.exploration_id=$1 AND initialized AND hot_ready AND hot_wave=topology_rev)
 AND NOT EXISTS(SELECT 1 FROM exploration_edges e JOIN exploration_nodes d ON d.id=e.src_id AND d.kind='digest' AND d.state='active'
 WHERE e.exploration_id=$1 AND e.dst_id=p.node_id AND e.rel='covers') ORDER BY p.node_id LIMIT $2`, s.expID, min(128, max(1, limit)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraphCandidate
	for rows.Next() {
		var v GraphCandidate
		if err = rows.Scan(&v.ID, &v.Version, &v.Summary, &v.Confidence, &v.Kind, &v.State, &v.GroupKey, &v.Hash); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) AdvanceSummaryCursor(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE graph_revisions SET summary_cursor=$2 WHERE exploration_id=$1`, s.expID, id)
	return err
}

func (s *ExplorationStore) RetireStaleGraphDigests(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, s.expID+8100000000); err != nil {
		return err
	}
	var pending bool
	if err = tx.QueryRowContext(ctx, `SELECT content_rev<>retired_content OR topology_rev<>retired_topology FROM graph_revisions WHERE exploration_id=$1`, s.expID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		return tx.Commit()
	}
	// Keep provenance edges for live authorization and stale-summary navigation.
	// Only active covering summaries participate in folding, so retired edges do
	// not prevent fresh publication or hide original nodes.
	result, err := tx.ExecContext(ctx, `WITH stale AS (SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='digest'
 AND state='active' AND NOT series_digest_sources_current(payload) ORDER BY id LIMIT 64)
 UPDATE exploration_nodes SET state='superseded' WHERE id IN(SELECT id FROM stale)`, s.expID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n < 64 {
		if _, err = tx.ExecContext(ctx, `UPDATE graph_revisions SET retired_content=content_rev,retired_topology=topology_rev WHERE exploration_id=$1`, s.expID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Lightweight anchor context; original evidence is deliberately not selected.
func (s *ExplorationStore) GraphSummaryAnchors(ctx context.Context, ids []int64) ([]GraphCandidate, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT p.node_id,p.source_version,left(p.summary,240),p.confidence,p.kind,p.state,p.source_hash FROM exploration_edges e
 JOIN graph_node_projection p ON p.node_id=e.src_id WHERE e.exploration_id=$1 AND e.dst_id=ANY($2::bigint[])
 AND e.rel<>'covers' AND NOT(e.src_id=ANY($2::bigint[])) GROUP BY p.node_id ORDER BY p.node_id LIMIT 8`, s.expID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraphCandidate
	for rows.Next() {
		var v GraphCandidate
		if err = rows.Scan(&v.ID, &v.Version, &v.Summary, &v.Confidence, &v.Kind, &v.State, &v.Hash); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *ExplorationStore) GraphSummaryAllowed(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL
 AND execution_mode='managed' AND status IN ('created','running') AND NOT paused AND NOT queued)`, s.expID).Scan(&ok)
	return ok, err
}

func (s *ExplorationStore) SavePlannerTodos(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO planner_working_state(exploration_id,todos) VALUES($1,$2) ON CONFLICT(exploration_id)
 DO UPDATE SET todos=EXCLUDED.todos,revision=planner_working_state.revision+1`, s.expID, string(raw))
	return err
}
func (s *ExplorationStore) PlannerTodos(ctx context.Context) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT todos FROM planner_working_state WHERE exploration_id=$1`, s.expID).Scan(&raw)
	if err == sql.ErrNoRows {
		return json.RawMessage(`[]`), nil
	}
	return raw, err
}

type PlannerTodoPreview struct {
	Index           int
	Status, Content string
	TotalChars      int
}

func (s *ExplorationStore) PlannerTodoPage(ctx context.Context, index, offset int) ([]PlannerTodoPreview, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT (a.ordinality-1)::int,COALESCE(a.value->>'status',''),
 substring(COALESCE(a.value->>'content','') FROM CASE WHEN a.ordinality=$2+1 THEN $3+1 ELSE 1 END FOR 500),
 char_length(COALESCE(a.value->>'content','')),jsonb_array_length(todos)
 FROM planner_working_state CROSS JOIN LATERAL jsonb_array_elements(todos) WITH ORDINALITY a(value,ordinality)
 WHERE exploration_id=$1 AND a.ordinality>$2 ORDER BY a.ordinality LIMIT 20`, s.expID, index, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []PlannerTodoPreview{}
	total := index
	for rows.Next() {
		var item PlannerTodoPreview
		if err = rows.Scan(&item.Index, &item.Status, &item.Content, &item.TotalChars, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func graphReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

// Cache metadata, never an execution authorization. Callers key it with the
// exact role policy and the live revision vector and still recheck read access.
func (s *ExplorationStore) GraphCountCache(ctx context.Context, key, rev string) ([]PlannerCount, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT counts FROM graph_count_cache WHERE exploration_id=$1 AND policy_key=$2 AND revision=$3`, s.expID, key, rev).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []PlannerCount
	err = json.Unmarshal(raw, &out)
	return out, err == nil, err
}
func (s *ExplorationStore) SaveGraphCounts(ctx context.Context, key, rev string, counts []PlannerCount) error {
	raw, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO graph_count_cache(exploration_id,policy_key,revision,counts) VALUES($1,$2,$3,$4)
 ON CONFLICT(exploration_id,policy_key) DO UPDATE SET revision=EXCLUDED.revision,counts=EXCLUDED.counts`, s.expID, key, rev, string(raw))
	return err
}

func (s *ExplorationStore) GraphVersions(ctx context.Context, ids []int64) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id,source_version FROM graph_node_projection WHERE exploration_id=$1 AND node_id=ANY($2::bigint[])`, s.expID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, v int64
		if err = rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// Source versions and a task-scoped advisory lock make publication atomic.
// Readers also validate versions, closing the race with a later source edit.
func (s *ExplorationStore) PublishGraphDigest(ctx context.Context, payload map[string]any, members []int64, versions map[int64]int64, revision string, expected ...map[int64]string) (int64, error) {
	if len(members) < 2 || len(members) > 32 || len(versions) < len(members) || len(versions) > 40 {
		return 0, fmt.Errorf("invalid bounded summary source set")
	}
	seen := map[int64]bool{}
	for _, id := range members {
		if id <= 0 || seen[id] || versions[id] <= 0 {
			return 0, fmt.Errorf("invalid summary member identity")
		}
		seen[id] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock only this task and its direct authorization sources, in stable order.
	// Unrelated content changes must not discard an otherwise valid summary.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(id+8100000000) FROM (
 SELECT $1::bigint id UNION SELECT t.exploration_id FROM tasks c JOIN task_relations r ON r.task_id=c.id
 JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL WHERE c.exploration_id=$1) q ORDER BY id`, s.expID); err != nil {
		return 0, err
	}
	// Existing writers may lock rows before their graph trigger, while archive
	// cleanup can already hold the graph lock. Never wait for a source/parent
	// row under this advisory lock: a busy source aborts this publication and
	// remains available as original data for a later bounded summary job.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM explorations WHERE id IN (
 SELECT $1::bigint UNION SELECT t.exploration_id FROM tasks c JOIN task_relations r ON r.task_id=c.id
 JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL WHERE c.exploration_id=$1
 ) ORDER BY id FOR KEY SHARE NOWAIT`, s.expID); err != nil {
		return 0, err
	}
	ids := []int64{}
	for id := range versions {
		ids = append(ids, id)
	}
	if _, err = tx.ExecContext(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND id=ANY($2::bigint[]) ORDER BY id FOR KEY SHARE NOWAIT`, s.expID, ids); err != nil {
		return 0, err
	}
	var rawVersions []byte
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object('id',node_id,'version',source_version,'hash',source_hash)),'[]'::jsonb)
 FROM graph_node_projection WHERE exploration_id=$1 AND node_id=ANY($2::bigint[])`, s.expID, ids).Scan(&rawVersions); err != nil {
		return 0, err
	}
	var current []struct {
		ID      int64  `json:"id"`
		Version int64  `json:"version"`
		Hash    string `json:"hash"`
	}
	if err = json.Unmarshal(rawVersions, &current); err != nil {
		return 0, err
	}
	if len(current) != len(versions) {
		return 0, fmt.Errorf("summary sources unavailable")
	}
	fingerprints := map[int64]string{}
	for _, n := range current {
		if n.Version != versions[n.ID] {
			return 0, fmt.Errorf("summary source changed")
		}
		if len(expected) > 0 && expected[0][n.ID] != n.Hash {
			return 0, fmt.Errorf("summary source fingerprint changed")
		}
		fingerprints[n.ID] = n.Hash
	}
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id WHERE n.exploration_id=$1 AND n.id=ANY($2::bigint[])`
	authQuery := contextVisibility(roots, false) + `, hot(id) AS (
 SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running','paused')
 UNION SELECT e.src_id FROM hot h JOIN exploration_edges e ON e.exploration_id=$1 AND e.dst_id=h.id AND e.rel<>'covers'
 ), active_children AS (SELECT e.dst_id id FROM exploration_edges e JOIN exploration_nodes p ON p.id=e.src_id
 WHERE e.exploration_id=$1 AND e.rel<>'covers' AND p.kind='intent' AND p.state IN ('open','running','paused'))
 SELECT count(*) FROM visible WHERE $3::bigint=0 AND $4::bigint=0 AND NOT EXISTS(SELECT 1 FROM hot WHERE id=ANY($6::bigint[]))
 AND NOT EXISTS(SELECT 1 FROM active_children WHERE id=ANY($6::bigint[]))`
	var readable int
	if err = tx.QueryRowContext(ctx, authQuery, s.expID, ids, int64(0), int64(0), int64(0), members).Scan(&readable); err != nil {
		return 0, err
	}
	if readable != len(ids) {
		return 0, fmt.Errorf("summary source is unauthorized or became hot")
	}
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE exploration_id=$1 AND execution_mode='managed' AND status IN ('created','running') AND NOT paused AND NOT queued AND deleted_at IS NULL)`, s.expID).Scan(&allowed); err != nil {
		return 0, err
	}
	if !allowed {
		return 0, fmt.Errorf("task no longer permits summary generation")
	}
	var covered bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_edges e JOIN exploration_nodes d ON d.id=e.src_id AND d.kind='digest' AND d.state='active' WHERE e.exploration_id=$1 AND e.rel='covers' AND e.dst_id=ANY($2::bigint[]))`, s.expID, members).Scan(&covered); err != nil {
		return 0, err
	}
	if covered {
		return 0, fmt.Errorf("source already summarized")
	}
	var generation int64
	if err = tx.QueryRowContext(ctx, `SELECT generation FROM graph_revisions WHERE exploration_id=$1`, s.expID).Scan(&generation); err != nil {
		return 0, err
	}
	payload["projection_generation"] = generation
	payload["source_exploration_id"] = s.expID
	payload["source_versions"] = versions
	payload["source_fingerprints"] = fingerprints
	payload["source_revision"] = revision
	payload["summary_schema_version"] = 2
	payload["body_is_summary"] = true
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var id int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO exploration_nodes(exploration_id,kind,state,payload,origin) VALUES($1,'digest','active',$2,'compactor') RETURNING id`, s.expID, string(raw)).Scan(&id); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) SELECT $1,$2,'covers',x FROM unnest($3::bigint[])x`, s.expID, id, members)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *ExplorationStore) GraphAccessRevision(ctx context.Context) (string, error) {
	ctx, cancel := graphReadContext(ctx)
	defer cancel()
	query := strings.Replace(graphRevisionSQL, "COALESCE(r.content_rev,0),COALESCE(r.topology_rev,0),COALESCE(r.access_rev,0)", "0,0,COALESCE(r.access_rev,0)", 1)
	var rev string
	err := s.db.QueryRowContext(ctx, query, s.expID, false).Scan(&rev)
	return rev, err
}

// Preserve the authorization epoch observed before a projected count query.
// A later live access read must never relabel old counts as a fresh snapshot.
func graphAccessPart(revision string) string {
	rows := strings.Split(revision, "|")
	for i, row := range rows {
		parts := strings.Split(row, ":")
		if len(parts) != 6 {
			return ""
		}
		parts[2], parts[3], parts[5] = "0", "0", "0"
		rows[i] = strings.Join(parts, ":")
	}
	return strings.Join(rows, "|")
}
