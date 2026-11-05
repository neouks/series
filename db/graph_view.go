package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// UI reads keep the existing administrator visibility. Inherited live intents
// remain private, and only live direct source relations enter this scope.
const graphUIScopeSQL = `WITH scope AS MATERIALIZED (
 SELECT t.exploration_id exp_id,t.id task_id,false inherited FROM tasks t WHERE t.exploration_id=$1 AND t.deleted_at IS NULL
 UNION ALL SELECT s.exploration_id,s.id,true FROM tasks c JOIN task_relations r ON r.task_id=c.id
 JOIN tasks s ON s.id=r.source_task_id AND s.deleted_at IS NULL
 WHERE c.exploration_id=$1 AND c.deleted_at IS NULL
), visible AS NOT MATERIALIZED (
 SELECT n.id,n.exploration_id,sc.task_id,sc.inherited FROM scope sc JOIN exploration_nodes n ON n.exploration_id=sc.exp_id
 WHERE NOT sc.inherited OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped')
) `

const graphUIColumns = `n.id,left(n.kind,40),n.priority,left(n.state,40),left(COALESCE(n.origin,''),80),n.created_at,
 left(` + nodeNavigationDisplaySQL + `,160),v.task_id,v.inherited`

// The page is already bounded, but an ordinary join can still choose a hash
// join which scans unrelated raw nodes. Keep both raw-row and cache lookups
// behind each selected ID. System columns validate the derived preview.
const graphUIPageRowSQL = ` CROSS JOIN LATERAL (
 SELECT n.*,n.xmin,n.ctid FROM exploration_nodes n WHERE n.id=v.id OFFSET 0
) n ` + nodeNavigationJoinSQL

type GraphViewNode struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Priority     int       `json:"priority"`
	State        string    `json:"state"`
	Origin       string    `json:"origin"`
	TS           time.Time `json:"ts"`
	Summary      string    `json:"summary"`
	SourceTaskID string    `json:"source_task_id,omitempty"`
	Inherited    bool      `json:"inherited,omitempty"`
	Preview      bool      `json:"preview"`
}

type GraphViewQuery struct {
	Focus, Before, Upper int64
	Limit                int
	Revision             string
}

type GraphViewPage struct {
	Revision       string          `json:"revision"`
	Unchanged      bool            `json:"unchanged,omitempty"`
	Nodes          []GraphViewNode `json:"nodes,omitempty"`
	Edges          []GraphViewEdge `json:"edges,omitempty"`
	HasMore        bool            `json:"has_more"`
	NextBefore     string          `json:"next_before,omitempty"`
	Upper          string          `json:"upper"`
	EdgesTruncated bool            `json:"edges_truncated"`
}

type GraphViewEdge struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

var ErrGraphNodeUnavailable = errors.New("graph node unavailable")
var ErrGraphBodyChanged = errors.New("graph node body changed; restart from offset 0")

func scanGraphUINode(scan interface{ Scan(...any) error }) (GraphViewNode, error) {
	var n GraphViewNode
	var id, task int64
	err := scan.Scan(&id, &n.Type, &n.Priority, &n.State, &n.Origin, &n.TS, &n.Summary, &task, &n.Inherited)
	n.ID, n.Preview = strconv.FormatInt(id, 10), true
	if n.Inherited {
		n.SourceTaskID = strconv.FormatInt(task, 10)
	}
	return n, err
}

// GraphView projects only one global window, including all directly related
// sources. Revision, page and edges share a repeatable-read snapshot; a matching
// revision avoids all node/body/edge reads. No exact whole-graph count is needed.
func (s *ExplorationStore) GraphView(ctx context.Context, q GraphViewQuery) (GraphViewPage, error) {
	out := GraphViewPage{}
	if q.Limit < 2 || q.Limit > 400 || q.Focus < 0 || q.Before < 0 || q.Upper < 0 || (q.Before > 0 && q.Upper == 0) {
		return out, fmt.Errorf("invalid graph window")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if q.Focus > 0 {
		var found bool
		if err = tx.QueryRowContext(ctx, graphUIScopeSQL+`SELECT EXISTS(SELECT 1 FROM visible WHERE id=$2)`, s.expID, q.Focus).Scan(&found); err != nil {
			return out, err
		}
		if !found {
			return out, ErrGraphNodeUnavailable
		}
	}
	var revision string
	if err = tx.QueryRowContext(ctx, `SELECT r.revision||':navigation:'||series_graph_navigation_rule()::text FROM (`+graphRevisionSQL+`) r(revision)`, s.expID, true).Scan(&revision); err != nil {
		return out, err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("graph-ui-v1:%d:%d:%d:%d:%d:%s", s.expID, q.Focus, q.Before, q.Upper, q.Limit, revision)))
	out.Revision, out.Upper = hex.EncodeToString(sum[:]), strconv.FormatInt(q.Upper, 10)
	if out.Revision == q.Revision {
		out.Unchanged = true
		return out, nil
	}
	if q.Upper == 0 {
		// Per-source indexed maximum, not a count or full visible-node scan.
		if err = tx.QueryRowContext(ctx, graphUIScopeSQL+`SELECT COALESCE(max(x.id),0) FROM scope sc CROSS JOIN LATERAL (
 SELECT id FROM exploration_nodes n WHERE n.exploration_id=sc.exp_id
 AND (NOT sc.inherited OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped')) ORDER BY id DESC LIMIT 1) x`, s.expID).Scan(&q.Upper); err != nil {
			return out, err
		}
		out.Upper = strconv.FormatInt(q.Upper, 10)
	}
	// Limit ids before projecting JSON fields; never send complete payloads.
	pageSQL := graphUIScopeSQL + `, page AS MATERIALIZED (
 SELECT v.* FROM scope sc CROSS JOIN LATERAL (
 SELECT n.id,n.exploration_id,sc.task_id,sc.inherited FROM exploration_nodes n
 WHERE n.exploration_id=sc.exp_id AND $2::bigint=0 AND n.id<=$4 AND ($3::bigint=0 OR n.id<$3)
 AND (NOT sc.inherited OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))
 ORDER BY n.id DESC LIMIT $5) v ORDER BY v.id DESC LIMIT $5
 ) SELECT ` + graphUIColumns + ` FROM page v ` + graphUIPageRowSQL + ` ORDER BY v.id DESC`
	if q.Focus > 0 {
		// OFFSET 0 keeps each eligibility lookup behind its indexed edge. An
		// unrestricted join lets PostgreSQL drive the query from every graph
		// node, even when only three neighbours exist. Group in index order so
		// multiple relationship kinds cannot consume the neighbour limit.
		pageSQL = graphUIScopeSQL + `, focus_scope AS MATERIALIZED (
 SELECT * FROM visible WHERE id=$2
 ), nearby AS MATERIALIZED (
 SELECT x.id FROM focus_scope f CROSS JOIN LATERAL (
 SELECT e.dst_id id FROM exploration_edges e CROSS JOIN LATERAL (
 SELECT 1 FROM exploration_nodes n WHERE n.id=e.dst_id AND n.exploration_id=e.exploration_id
 AND (NOT f.inherited OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped')) OFFSET 0) allowed
 WHERE e.exploration_id=f.exploration_id AND e.src_id=f.id AND e.dst_id<=$4 AND ($3::bigint=0 OR e.dst_id<$3)
 GROUP BY e.dst_id ORDER BY e.dst_id DESC LIMIT $5) x
 UNION SELECT x.id FROM focus_scope f CROSS JOIN LATERAL (
 SELECT e.src_id id FROM exploration_edges e CROSS JOIN LATERAL (
 SELECT 1 FROM exploration_nodes n WHERE n.id=e.src_id AND n.exploration_id=e.exploration_id
 AND (NOT f.inherited OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped')) OFFSET 0) allowed
 WHERE e.exploration_id=f.exploration_id AND e.dst_id=f.id AND e.src_id<=$4 AND ($3::bigint=0 OR e.src_id<$3)
 GROUP BY e.src_id ORDER BY e.src_id DESC LIMIT $5) x
 UNION SELECT id FROM focus_scope
 ), page AS MATERIALIZED (
 SELECT i.id,f.exploration_id,f.task_id,f.inherited FROM nearby i CROSS JOIN focus_scope f
 ORDER BY (i.id=$2) DESC,i.id DESC LIMIT $5
 ) SELECT ` + graphUIColumns + ` FROM page v ` + graphUIPageRowSQL + ` ORDER BY (v.id=$2) DESC,v.id DESC`
	}
	rows, err := tx.QueryContext(ctx, pageSQL, s.expID, q.Focus, q.Before, q.Upper, q.Limit+1)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		n, e := scanGraphUINode(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		out.Nodes = append(out.Nodes, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Nodes) > q.Limit {
		out.Nodes, out.HasMore = out.Nodes[:q.Limit], true
	}
	// Limits are global across sources and include JSON escaping. Keep complete
	// records; a shortened window's cursor resumes after its last retained id.
	bytes := 0
	for i, n := range out.Nodes {
		encoded, _ := json.Marshal(n)
		if bytes+len(encoded)+1 > 192<<10 {
			out.Nodes, out.HasMore = out.Nodes[:i], true
			break
		}
		bytes += len(encoded) + 1
	}
	ids := make([]int64, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		id, _ := strconv.ParseInt(n.ID, 10, 64)
		ids = append(ids, id)
		if id != q.Focus {
			out.NextBefore = n.ID
		}
	}
	if !out.HasMore {
		out.NextBefore = ""
	}
	if len(ids) > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT e.src_id,e.dst_id,left(e.rel,40) FROM exploration_edges e
 JOIN exploration_nodes a ON a.id=e.src_id AND a.exploration_id=e.exploration_id
 JOIN exploration_nodes b ON b.id=e.dst_id AND b.exploration_id=e.exploration_id
 WHERE e.src_id=ANY($1::bigint[]) AND e.dst_id=ANY($1::bigint[])
 ORDER BY e.src_id,e.dst_id,e.rel LIMIT 801`, ids)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var a, b int64
			var rel string
			if err = rows.Scan(&a, &b, &rel); err != nil {
				rows.Close()
				return out, err
			}
			out.Edges = append(out.Edges, GraphViewEdge{strconv.FormatInt(a, 10), strconv.FormatInt(b, 10), rel})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		if len(out.Edges) > 800 {
			out.Edges, out.EdgesTruncated = out.Edges[:800], true
		}
		bytes = 0
		for i, e := range out.Edges {
			encoded, _ := json.Marshal(e)
			if bytes+len(encoded)+1 > 48<<10 {
				out.Edges, out.EdgesTruncated = out.Edges[:i], true
				break
			}
			bytes += len(encoded) + 1
		}
	}
	return out, nil
}

type GraphNodeBody struct {
	Node        GraphViewNode `json:"node"`
	Payload     string        `json:"payload"`
	NextOffset  int           `json:"next_offset"`
	BodyVersion string        `json:"body_version"`
}

// Body chunks do not append indefinitely in the UI. The body version guards
// continuation against mixing chunks from different revisions of one node.
func (s *ExplorationStore) GraphNodeBody(ctx context.Context, id int64, offset int, version string) (GraphNodeBody, error) {
	out := GraphNodeBody{NextOffset: -1}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var nid, task int64
	n := &out.Node
	err := s.db.QueryRowContext(ctx, graphUIScopeSQL+`SELECT `+graphUIColumns+`,
 substring(n.payload::text FROM $3+1 FOR 16001),md5(n.id::text||':'||n.xmin::text)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id `+nodeNavigationJoinSQL+` WHERE v.id=$2`, s.expID, id, offset).Scan(
		&nid, &n.Type, &n.Priority, &n.State, &n.Origin, &n.TS, &n.Summary, &task, &n.Inherited, &out.Payload, &out.BodyVersion)
	if err == sql.ErrNoRows {
		return out, ErrGraphNodeUnavailable
	}
	if err != nil {
		return out, err
	}
	if offset < 0 || (offset > 0 && version == "") || (version != "" && version != out.BodyVersion) {
		return GraphNodeBody{}, ErrGraphBodyChanged
	}
	n.ID, n.Preview = strconv.FormatInt(nid, 10), true
	if n.Inherited {
		n.SourceTaskID = strconv.FormatInt(task, 10)
	}
	if chars := []rune(out.Payload); len(chars) > 16000 {
		out.Payload = string(chars[:16000])
		out.NextOffset = offset + 16000
	}
	return out, nil
}
