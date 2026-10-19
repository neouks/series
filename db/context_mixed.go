package db

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Separate ID bounds freeze both streams, even when their sequences are very
// different. Time, ID and source together provide a unique stable ordering.
type ContextMixedCursor struct {
	NodeUpper     int64     `json:"n"`
	ActivityUpper int64     `json:"a"`
	Time          time.Time `json:"t"`
	ID            int64     `json:"i"`
	Source        string    `json:"k"`
}

func (c ContextMixedCursor) Valid() bool {
	if c.NodeUpper < 0 || c.ActivityUpper < 0 || c.ID <= 0 || c.Time.IsZero() {
		return false
	}
	return (c.Source == "node" && c.ID <= c.NodeUpper) || (c.Source == "activity" && c.ID <= c.ActivityUpper)
}

// SearchMixedContext executes one bounded statement/snapshot. Each arm applies
// the existing live authorization before paging. Only at most limit+1 hits per
// stream enter the final merge; no complete transcripts are loaded into Go.
func (s *ExplorationStore) SearchMixedContext(ctx context.Context, q ContextSearch, cursor *ContextMixedCursor) ([]ContextHit, *ContextMixedCursor, error) {
	if q.Limit < 1 || q.Limit > 50 || len(q.Types) < 2 || !slices.Contains(q.Types, "activity") || (cursor != nil && !cursor.Valid()) {
		return nil, nil, fmt.Errorf("无效混合检索参数或游标")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var nodeUpper, activityUpper, beforeTime any
	var beforeID int64
	var beforeSource string
	if cursor != nil {
		nodeUpper, activityUpper, beforeTime = cursor.NodeUpper, cursor.ActivityUpper, cursor.Time
		beforeID, beforeSource = cursor.ID, cursor.Source
	}
	query := contextScopeSQL + `, bounds AS MATERIALIZED (
 SELECT COALESCE($12::bigint,(SELECT max(id) FROM exploration_nodes WHERE exploration_id IN (SELECT exp FROM contexts)),0) node_upper,
 COALESCE($13::bigint,(SELECT max(id) FROM activity WHERE exploration_id IN (SELECT exp FROM contexts)),0) activity_upper
 ) SELECT hits.*,bounds.node_upper,bounds.activity_upper FROM (
 SELECT * FROM (` + s.contextSearchSQL(false, true) + `) nodes
 UNION ALL SELECT * FROM (` + s.contextSearchSQL(true, true) + `) activities
 ) hits CROSS JOIN bounds WHERE $11::bigint=0
 ORDER BY hits.created_at DESC,hits.id DESC,hits.source DESC LIMIT $7`
	rows, err := s.db.QueryContext(ctx, query, s.expID, q.Types, q.Query, q.Severity, q.AssetID, int64(0), q.Limit+1, true, q.State, q.IntentID, int64(0), nodeUpper, activityUpper, beforeTime, beforeID, beforeSource)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	hits := []ContextHit{}
	var next *ContextMixedCursor
	for rows.Next() {
		var h ContextHit
		var c ContextMixedCursor
		if err := rows.Scan(&h.Ref, &h.ID, &h.Kind, &h.NodeID, &h.TaskID, &h.Inherited, &h.State, &h.Summary, &h.Field, &h.Snippet, &h.MatchOffset, &h.FindingID, &c.Time, &c.Source, &c.NodeUpper, &c.ActivityUpper); err != nil {
			return nil, nil, err
		}
		c.ID = h.ID
		if h.Kind == "finding" {
			h.FindingNodeID = h.NodeID
		}
		hits = append(hits, h)
		if len(hits) <= q.Limit {
			next = &c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(hits) <= q.Limit {
		next = nil
	}
	return hits, next, nil
}
