package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ContextSearch scopes a lexical query to authorized nodes before pagination.
// Activity searches use the same lineage permissions and never include reasoning
// or a main/planner transcript. IDs are stable cursors; no count(*) is needed.
type ContextSearch struct {
	Types                            []string
	Query, State, Severity           string
	AssetID, IntentID, Before, Upper int64
	Limit                            int
}
type ContextHit struct {
	Ref           string `json:"ref"`
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	NodeID        int64  `json:"node_id"`
	TaskID        int64  `json:"source_task_id"`
	Inherited     bool   `json:"inherited,omitempty"`
	State         string `json:"state,omitempty"`
	Summary       string `json:"summary"`
	Field         string `json:"match_field,omitempty"`
	Snippet       string `json:"snippet,omitempty"`
	FindingID     int64  `json:"finding_id,omitempty"`
	FindingNodeID int64  `json:"finding_node_id,omitempty"`
}

// Keep the lineage/asset policy shared with existing tool reads. The roots are
// supplied by each query, so a narrow lookup doesn't traverse the entire graph.
func contextVisibility(roots string, worker bool) string {
	at := strings.Index(toolNodeVisibilitySQL, "), lineage(root,id,exp)")
	lineage := toolNodeVisibilitySQL[at:]
	if !worker {
		// Match TaskNodeAccess: a digest without members has unverifiable
		// provenance, including when it occurs in an ordinary node's ancestry.
		lineage = strings.Replace(lineage, "AND n.id IS NULL)", "AND (n.id IS NULL OR (n.kind='digest' AND NOT EXISTS(SELECT 1 FROM exploration_edges de WHERE de.exploration_id=n.exploration_id AND de.src_id=n.id AND de.rel='covers'))))", 1)
	}
	return workerReadSQL(`WITH RECURSIVE current_task AS (
 SELECT id FROM tasks WHERE exploration_id=$1 AND deleted_at IS NULL
 ), contexts AS (
 SELECT $1::bigint exp,COALESCE((SELECT id FROM current_task),0) owner
 WHERE EXISTS(SELECT 1 FROM current_task) OR NOT EXISTS(SELECT 1 FROM tasks WHERE exploration_id=$1)
 UNION SELECT t.exploration_id,t.id FROM task_relations r JOIN current_task c ON c.id=r.task_id
 JOIN tasks t ON t.id=r.source_task_id AND t.deleted_at IS NULL
 ), roots AS MATERIALIZED (`+roots+lineage, worker)
}

// Evidence and platform IDs must survive projection without float64 rounding.
func decodeContextJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

const contextNodeFields = `VALUES
 ('/summary',COALESCE(n.payload->>'summary','')),
 ('/name',COALESCE(n.payload->>'name','')),
 ('/detail',COALESCE(n.payload->>'detail','')),
 ('/evidence',COALESCE(n.payload->>'evidence','')),
 ('/text',COALESCE(n.payload->>'text','')),
 ('/body',COALESCE(n.payload->>'body','')),
 ('/report',CASE WHEN n.kind='finding' THEN COALESCE((SELECT f.report FROM findings f WHERE f.node_id=n.id LIMIT 1),'') ELSE '' END)`

func (s *ExplorationStore) ContextUpper(ctx context.Context, activity bool) (int64, error) {
	table := "exploration_nodes"
	if activity {
		table = "activity"
	}
	var id int64
	// The upper bound is not exposed as a count and cannot grant access.
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(id),0) FROM `+table).Scan(&id)
	return id, err
}

func (s *ExplorationStore) SearchContext(ctx context.Context, q ContextSearch) ([]ContextHit, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	activity := len(q.Types) == 1 && q.Types[0] == "activity"
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner
 FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.kind=ANY($2::text[]) AND ($4='' OR n.payload->>'severity'=$4)
 AND ($9='' OR n.state=$9)
 AND (n.exploration_id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))`
	types := q.Types
	if activity {
		types = []string{"intent"}
		roots += ` AND ($10::bigint=0 OR n.id=$10)`
	} else {
		roots += ` AND n.id<=$11 AND ($6::bigint=0 OR n.id<$6)
 AND ($10::bigint=0 OR n.id=$10 OR EXISTS(SELECT 1 FROM exploration_edges e WHERE e.exploration_id=n.exploration_id AND e.src_id=$10 AND e.dst_id=n.id AND e.rel='yields'))
 AND ($3='' OR EXISTS(SELECT 1 FROM (` + contextNodeFields + `) f(path,body) WHERE strpos(lower(f.body),lower($3))>0))`
	}
	sql := contextVisibility(roots, s.workerRead)
	if activity {
		sql += ` SELECT 'activity:'||a.id,a.id,'activity',n.id,n.owner,n.exploration_id<>$1,n.state,
 left(COALESCE(a.summary,''),240),COALESCE(hit.path,''),COALESCE(hit.snippet,''),0::bigint
 FROM visible n JOIN activity a ON a.node_id=n.id AND a.exploration_id=n.exploration_id
 LEFT JOIN LATERAL (SELECT path,substring(body FROM greatest(1,strpos(lower(body),lower($3))-60) FOR 180) snippet FROM (VALUES ('/detail',COALESCE(a.detail,'')),('/summary',COALESCE(a.summary,''))) f(path,body) WHERE $3<>'' AND strpos(lower(body),lower($3))>0 LIMIT 1) hit ON true
 WHERE a.kind NOT IN ('thinking','usage') AND a.id<=$11 AND ($6::bigint=0 OR a.id<$6)
 AND ($3='' OR hit.path IS NOT NULL)
 AND $8::boolean ORDER BY a.id DESC LIMIT $7`
	} else {
		sql += ` SELECT 'node:'||n.id,n.id,n.kind,n.id,v.owner,n.exploration_id<>$1,n.state,left(COALESCE(n.payload->>'summary',n.payload->>'text',n.payload->>'body',''),240),
 COALESCE(hit.path,''),COALESCE(hit.snippet,''),CASE WHEN n.kind='finding' THEN COALESCE((SELECT f.id FROM findings f WHERE f.node_id=n.id LIMIT 1),0) ELSE 0 END
 FROM visible v JOIN exploration_nodes n ON n.id=v.id
 LEFT JOIN LATERAL (SELECT path,substring(body FROM greatest(1,strpos(lower(body),lower($3))-60) FOR 180) snippet
 FROM (` + contextNodeFields + `) f(path,body) WHERE $3<>'' AND strpos(lower(body),lower($3))>0 LIMIT 1) hit ON true
 WHERE $8::boolean ORDER BY n.id DESC LIMIT $7`
	}
	rows, err := s.db.QueryContext(ctx, sql, s.expID, types, q.Query, q.Severity, q.AssetID, q.Before, q.Limit+1, true, q.State, q.IntentID, q.Upper)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ContextHit{}
	for rows.Next() {
		var h ContextHit
		if err = rows.Scan(&h.Ref, &h.ID, &h.Kind, &h.NodeID, &h.TaskID, &h.Inherited, &h.State, &h.Summary, &h.Field, &h.Snippet, &h.FindingID); err != nil {
			return nil, err
		}
		if h.Kind == "finding" {
			h.FindingNodeID = h.NodeID
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ContextNode projects at the database boundary. Long children are represented
// by JSON pointers, never fetched in full just to truncate them in Go.
func (s *ExplorationStore) ContextNode(ctx context.Context, id int64, field string, index, offset, budget int) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id WHERE n.id=$2
 AND (n.exploration_id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))`
	path := []string{}
	if field != "" {
		if !strings.HasPrefix(field, "/") {
			return nil, fmt.Errorf("field 必须为 JSON Pointer")
		}
		for _, p := range strings.Split(field[1:], "/") {
			path = append(path, strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~"))
		}
	}
	query := contextVisibility(roots, s.workerRead) + `, content AS (
 SELECT n.id,n.kind,n.state,v.owner,n.exploration_id<>$1 inherited,
 CASE WHEN n.kind='finding' THEN COALESCE((SELECT f.id FROM findings f WHERE f.node_id=n.id LIMIT 1),0) ELSE 0 END finding_id,
 (CASE WHEN n.kind='finding' THEN n.payload||jsonb_build_object('report',COALESCE((SELECT f.report FROM findings f WHERE f.node_id=n.id LIMIT 1),'')) ELSE n.payload END)#>$3::text[] value
 FROM visible v JOIN exploration_nodes n ON n.id=v.id
 ), children AS (
 SELECT child.key,child.value, row_number() OVER(ORDER BY child.key) seq FROM content c CROSS JOIN LATERAL jsonb_each(CASE WHEN jsonb_typeof(c.value)='object' THEN c.value ELSE '{}'::jsonb END) child
 UNION ALL SELECT (child.ordinality-1)::text,child.value,child.ordinality FROM content c CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(c.value)='array' THEN c.value ELSE '[]'::jsonb END) WITH ORDINALITY child
 ), page AS (SELECT * FROM children ORDER BY seq OFFSET $4 LIMIT 20)
 SELECT jsonb_build_object('id',id,'node_id',id,'ref','node:'||id,'kind',kind,'state',state,'source_task_id',owner,'inherited',inherited,'finding_id',finding_id,'finding_node_id',CASE WHEN kind='finding' THEN id END,'payload',
 CASE WHEN value IS NULL THEN jsonb_build_object('error','字段不存在') WHEN jsonb_typeof(value)='string' THEN jsonb_build_object('value',substring(value#>>'{}' FROM $6+1 FOR $7),'total_chars',char_length(value#>>'{}'),'offset',$6,'truncated',char_length(value#>>'{}')>$6+$7,'next_offset',CASE WHEN char_length(value#>>'{}')>$6+$7 THEN $6+$7 END)
 WHEN jsonb_typeof(value) IN ('array','object') THEN jsonb_build_object('entries',COALESCE((SELECT jsonb_agg(
 CASE WHEN octet_length(p.value::text)<=$7/20 THEN jsonb_build_object('field',$8||'/'||replace(replace(key,'~','~0'),'/','~1'),'value',p.value)
 ELSE jsonb_build_object('field',$8||'/'||replace(replace(key,'~','~0'),'/','~1'),'deferred',true,'type',jsonb_typeof(p.value)) END ORDER BY seq) FROM page p),'[]'::jsonb),'next_index',CASE WHEN (SELECT count(*) FROM children)>$4+20 THEN $4+20 END)
 ELSE jsonb_build_object('value',value) END)
 FROM content`
	// $5 belongs to the shared authorization CTE (asset filter).
	var raw []byte
	err := s.db.QueryRowContext(ctx, query, s.expID, id, path, index, int64(0), offset, budget, field).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = decodeContextJSON(raw, &out)
	return out, err
}

func (s *ExplorationStore) ContextActivity(ctx context.Context, id int64, offset, budget int, fields ...string) (map[string]any, error) {
	field := "/detail"
	if len(fields) > 0 && fields[0] != "" {
		field = fields[0]
	}
	if field != "/detail" && field != "/summary" {
		return nil, fmt.Errorf("activity field 仅支持 /detail 或 /summary")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 JOIN activity a ON a.node_id=n.id AND a.exploration_id=n.exploration_id
 WHERE a.id=$2 AND n.kind='intent' AND a.kind NOT IN ('thinking','usage')
 AND (n.exploration_id=$1 OR n.state IN ('done','blocked','exhausted','stopped'))`
	query := contextVisibility(roots, s.workerRead) + ` SELECT jsonb_build_object('ref','activity:'||a.id,'step_id',a.id,'node_id',v.id,'intent_id',v.id,'kind',a.kind,'tool',a.tool,'is_error',a.is_error,'source_task_id',v.owner,'inherited',a.exploration_id<>$1,
 'detail',substring(CASE WHEN $6='/summary' THEN COALESCE(a.summary,'') ELSE COALESCE(NULLIF(a.detail,''),a.summary,'') END FROM $3+1 FOR $4),'offset',$3,'total_chars',char_length(CASE WHEN $6='/summary' THEN COALESCE(a.summary,'') ELSE COALESCE(NULLIF(a.detail,''),a.summary,'') END),
 'next_offset',CASE WHEN char_length(CASE WHEN $6='/summary' THEN COALESCE(a.summary,'') ELSE COALESCE(NULLIF(a.detail,''),a.summary,'') END)>$3+$4 THEN $3+$4 END)
 FROM visible v JOIN activity a ON a.node_id=v.id AND a.exploration_id=v.exploration_id WHERE a.id=$2`
	var raw []byte
	err := s.db.QueryRowContext(ctx, query, s.expID, id, offset, budget, int64(0), field).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = decodeContextJSON(raw, &out)
	return out, err
}

// Output and permission are read in one statement, so a separate metadata
// lookup cannot authorize a later, unguarded fetch of Worker evidence.
func (s *ExplorationStore) ContextOutput(ctx context.Context, id int64, offset, budget int) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.id=$2 AND n.kind='intent' AND (n.exploration_id=$1 OR n.state IN ('done','blocked','exhausted','stopped'))`
	query := contextVisibility(roots, s.workerRead) + ` SELECT jsonb_strip_nulls(jsonb_build_object(
 'id',v.id,'node_id',v.id,'ref','node:'||v.id,'kind',v.kind,'state',v.state,'source_task_id',v.owner,'inherited',v.exploration_id<>$1,
 'has_output',a.id IS NOT NULL,'output_kind',a.kind,'is_final',COALESCE(a.kind='result' AND v.state IN ('done','blocked','exhausted','stopped'),false),
 'text',a.detail,'is_error',a.is_error,'activity_ref','activity:'||a.id,'total_chars',a.total_chars,
 'next_offset',CASE WHEN a.total_chars>$3+$4 THEN $3+$4 END))
 FROM visible v LEFT JOIN LATERAL (
 SELECT a.id,a.kind,a.is_error,substring(COALESCE(NULLIF(a.detail,''),a.summary,'') FROM $3+1 FOR $4) detail,
 char_length(COALESCE(NULLIF(a.detail,''),a.summary,'')) total_chars
 FROM activity a WHERE a.exploration_id=v.exploration_id AND a.node_id=v.id AND a.kind IN ('result','text')
 AND a.id>COALESCE((SELECT after_activity FROM worker_feedback_run WHERE intent_id=v.id),0)
 ORDER BY CASE WHEN a.kind='result' THEN 0 ELSE 1 END,a.id DESC LIMIT 1
 )a ON true`
	var raw []byte
	if err := s.db.QueryRowContext(ctx, query, s.expID, id, offset, budget, int64(0)).Scan(&raw); err != nil {
		return nil, err
	}
	var out map[string]any
	err := decodeContextJSON(raw, &out)
	return out, err
}

func (s *AssetStore) SearchUntestedToolAssets(taskID int64, dsl, typ string, limit, offset int) ([]*Asset, error) {
	node, err := ParseDSL(dsl)
	if err != nil {
		return nil, err
	}
	b := &whereBuilder{taskID: taskID, args: []any{taskID}}
	where, err := b.build(node)
	if err != nil {
		return nil, err
	}
	if typ != "" {
		b.args = append(b.args, typ)
		where += fmt.Sprintf(" AND type=$%d", len(b.args))
	}
	b.args = append(b.args, limit, offset)
	query := `WITH ` + contextCoverageCTE + ` ` + s.selectAssetColumns() + ` WHERE assets.id IN (SELECT target.id FROM target WHERE target.id NOT IN (SELECT asset_id FROM tested)) AND (` + where + `) ORDER BY assets.id LIMIT $` + fmt.Sprint(len(b.args)-1) + ` OFFSET $` + fmt.Sprint(len(b.args))
	rows, err := s.query(query, b.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanAssets(rows)
	if err != nil {
		return nil, err
	}
	if err = s.hydrateTaskAssetSources(taskID, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *ExplorationStore) ContextParents(ctx context.Context, intentID, before int64, limit int) ([]ContextHit, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id
 WHERE n.kind IN ('fact','finding') AND EXISTS(SELECT 1 FROM exploration_edges e JOIN exploration_nodes i ON i.id=e.dst_id AND i.exploration_id=e.exploration_id
 WHERE i.id=$2 AND i.kind='intent' AND i.exploration_id IN(SELECT exp FROM contexts) AND e.src_id=n.id AND e.rel='derived_from')`
	query := contextVisibility(roots, s.workerRead) + ` SELECT n.id,n.kind,v.owner,n.exploration_id<>$1,left(COALESCE(n.payload->>'summary',''),240)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id WHERE ($3::bigint=0 OR n.id<$3) AND $4::boolean ORDER BY n.id DESC LIMIT $6`
	rows, e := s.db.QueryContext(ctx, query, s.expID, intentID, before, true, int64(0), limit+1)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ContextHit{}
	for rows.Next() {
		var h ContextHit
		if e = rows.Scan(&h.ID, &h.Kind, &h.TaskID, &h.Inherited, &h.Summary); e != nil {
			return nil, e
		}
		h.NodeID = h.ID
		h.Ref = fmt.Sprintf("node:%d", h.ID)
		if h.Kind == "finding" {
			h.FindingNodeID = h.NodeID
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *ExplorationStore) EdgesWithin(ids []int64) ([]Edge, error) {
	if len(ids) == 0 {
		return []Edge{}, nil
	}
	rows, e := s.db.Query(`SELECT src_id,rel,dst_id FROM exploration_edges WHERE exploration_id=$1 AND src_id=ANY($2::bigint[]) AND dst_id=ANY($2::bigint[])`, s.expID, ids)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		var edge Edge
		if e = rows.Scan(&edge.From, &edge.Rel, &edge.To); e != nil {
			return nil, e
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// ContextDigestMembers retains the same live authorization snapshot as the
// digest itself, including every member and its ancestors. No full body fetch.
func (s *ExplorationStore) ContextDigestMembers(ctx context.Context, id, before int64, limit, summaryChars int) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id WHERE n.id=$2 AND n.kind='digest'`
	query := contextVisibility(roots, s.workerRead) + ` SELECT n.id,n.kind,jsonb_strip_nulls(jsonb_build_object('summary',left(n.payload->>'summary',$6),'confidence',n.payload->'confidence')),n.priority,n.state,COALESCE(n.origin,''),COALESCE(n.owner,''),COALESCE(n.blocked_reason,''),n.created_at
 FROM visible v JOIN exploration_edges e ON e.src_id=v.id AND e.exploration_id=v.exploration_id AND e.rel='covers' JOIN exploration_nodes n ON n.id=e.dst_id AND n.exploration_id=e.exploration_id WHERE ($3::bigint=0 OR n.id<$3) ORDER BY n.id DESC LIMIT $4`
	rows, e := s.db.QueryContext(ctx, query, s.expID, id, before, limit+1, int64(0), summaryChars)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ToolHotNodeIDs is the rendering-only hot set. Compression still loads full
// bodies when it needs them; an overview needs only causal reachability.
func (s *ExplorationStore) ToolHotNodeIDs(ctx context.Context) (map[int64]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, e := s.db.QueryContext(ctx, `WITH RECURSIVE live AS (SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running','paused')), hot(id) AS (
 SELECT id FROM live UNION SELECT e.src_id FROM hot h JOIN exploration_edges e ON e.dst_id=h.id AND e.exploration_id=$1 AND e.rel<>'covers'
 ) SELECT id FROM hot UNION SELECT e.dst_id FROM live l JOIN exploration_edges e ON e.src_id=l.id AND e.exploration_id=$1 AND e.rel<>'covers'`, s.expID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *ExplorationStore) ToolActiveDigests() ([]*Node, error) {
	rows, e := s.db.Query(`SELECT id,kind,jsonb_build_object('body',left(payload->>'body',1200)),priority,state,COALESCE(origin,''),COALESCE(owner,''),COALESCE(blocked_reason,''),created_at FROM exploration_nodes WHERE exploration_id=$1 AND kind='digest' AND state='active' ORDER BY id`, s.expID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	return scanNodes(rows)
}
