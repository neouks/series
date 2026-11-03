package db

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Fold in SQL, after both node and covering-digest authorization. Only the
// requested page leaves PostgreSQL; no global member/hot-node lists enter Go.
func (s *ExplorationStore) ToolUnfoldedPage(ctx context.Context, kind string, limit int, viewer int64) (ToolNodePage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	visibility := strings.Replace(toolNodeVisibilitySQL, "n.kind=$2", "(n.kind=$2 OR (n.kind='digest' AND n.state='active'))", 1)
	if kind == KindIntent {
		visibility = strings.Replace(visibility, toolIntentHistoryPredicate, "n.state IN ('done','blocked','exhausted','stopped','active')", 1)
	}
	query := workerReadSQL(visibility, s.workerRead) + `, allowed_visible AS (
 SELECT v.* FROM visible v WHERE $6::bigint=0 AND ($9::bigint=0 OR NOT EXISTS(SELECT 1 FROM anchors a WHERE a.root=v.id AND NOT task_asset_effectively_approved($9,a.asset_id)))
 ), hot(id) AS (
 SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state IN ('open','running','paused')
 UNION SELECT e.src_id FROM hot h JOIN exploration_edges e ON e.exploration_id=$1 AND e.dst_id=h.id AND e.rel<>'covers'
 ), active_children AS (
 SELECT e.dst_id id FROM exploration_edges e JOIN exploration_nodes p ON p.id=e.src_id WHERE e.exploration_id=$1
 AND e.rel<>'covers' AND p.kind='intent' AND p.state IN ('open','running','paused')
 ), page AS (
 SELECT v.id FROM allowed_visible v WHERE v.kind=$2 AND NOT EXISTS(SELECT 1 FROM exploration_edges e JOIN allowed_visible d ON d.id=e.src_id AND d.kind='digest'
 WHERE e.exploration_id=$1 AND e.dst_id=v.id AND e.rel='covers' AND NOT EXISTS(SELECT 1 FROM hot h WHERE h.id=v.id)
 AND NOT EXISTS(SELECT 1 FROM active_children h WHERE h.id=v.id)) ORDER BY v.id DESC LIMIT $7
 ) SELECT jsonb_build_object('total',(SELECT count(*) FROM allowed_visible WHERE kind=$2),'nodes',COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'id',n.id,'kind',n.kind,'state',n.state,'created_at',n.created_at,'priority',n.priority,'payload',jsonb_strip_nulls(jsonb_build_object(
 'summary',left(n.payload->>'summary',360),'confidence',left(n.payload->>'confidence',80),'severity',n.payload->>'severity','text',left(n.payload->>'text',500)))) ORDER BY n.id DESC)
 FROM page p JOIN exploration_nodes n ON n.id=p.id),'[]'::jsonb))`
	var raw []byte
	err := s.db.QueryRowContext(ctx, workerReadSQL(query, s.workerRead), s.expID, kind, "", "", int64(0), int64(0), limit, false, viewer).Scan(&raw)
	if err != nil {
		return ToolNodePage{}, err
	}
	var out ToolNodePage
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *ExplorationStore) ToolRunningIntentPage(ctx context.Context, limit int) ([]*Node, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	visibility := strings.Replace(toolNodeVisibilitySQL, toolIntentHistoryPredicate, "n.exploration_id=$1 AND n.state='running'", 1)
	rows, err := s.db.QueryContext(ctx, workerReadSQL(visibility, s.workerRead)+` SELECT `+toolCompactNodeCols+` FROM exploration_nodes WHERE id IN (SELECT id FROM visible) AND $6::bigint=0 ORDER BY id LIMIT $7`, s.expID, KindIntent, "", "", int64(0), int64(0), limit, false)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *ExplorationStore) ToolRecentDigests(ctx context.Context, limit int, viewer int64) ([]ToolDigestIndexRow, []int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := workerReadSQL(toolNodeVisibilitySQL, s.workerRead) + `, ranked AS (
 SELECT v.id,(SELECT max(dst_id) FROM exploration_edges WHERE exploration_id=$1 AND src_id=v.id AND rel='covers') freshness
 FROM visible v WHERE $6::bigint=0 AND ($10::bigint=0 OR NOT EXISTS(SELECT 1 FROM anchors a WHERE a.root=v.id AND NOT task_asset_effectively_approved($10,a.asset_id))) ORDER BY freshness DESC,id DESC LIMIT $7
 ), numbered AS (SELECT *,row_number() OVER(ORDER BY freshness DESC,id DESC) seq FROM ranked)
 SELECT jsonb_build_object('rows',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',n.id,'body',left(n.payload->>'body',1200),
 'body_truncated',char_length(COALESCE(n.payload->>'body',''))>1200,'source_revision',n.payload->>'source_revision','rule_version',CASE WHEN n.payload->>'summary_rule_version' ~ '^[0-9]{1,6}$' THEN (n.payload->>'summary_rule_version')::int ELSE 0 END,
 'member_count',(SELECT count(*) FROM exploration_edges WHERE exploration_id=$1 AND src_id=n.id AND rel='covers')) ORDER BY p.seq)
 FROM numbered p JOIN exploration_nodes n ON n.id=p.id WHERE p.seq<=$9),'[]'::jsonb),
 'more',COALESCE((SELECT jsonb_agg(id ORDER BY seq) FROM numbered WHERE seq>$9),'[]'::jsonb))`
	var raw []byte
	err := s.db.QueryRowContext(ctx, workerReadSQL(query, s.workerRead), s.expID, KindDigest, "", "", int64(0), int64(0), limit+50, false, limit, viewer).Scan(&raw)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Rows []ToolDigestIndexRow `json:"rows"`
		More []int64              `json:"more"`
	}
	err = json.Unmarshal(raw, &out)
	return out.Rows, out.More, err
}
