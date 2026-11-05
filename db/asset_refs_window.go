package db

import (
	"context"
	"fmt"
)

// AssetRefsWindow projects a bounded page, sharing the UI's direct-source scope.
func (s *ExplorationStore) AssetRefsWindow(ctx context.Context, asset, before int64, limit int) ([]AssetRef, bool, error) {
	if asset <= 0 || before < 0 || limit < 1 || limit > 100 {
		return nil, false, fmt.Errorf("invalid asset reference window")
	}
	rows, err := s.contextQuery(ctx, graphUIScopeSQL+`, page AS MATERIALIZED (
 SELECT v.* FROM visible v JOIN exploration_anchors a ON a.node_id=v.id JOIN exploration_nodes filter ON filter.id=v.id
 WHERE filter.kind IN ('intent','fact','finding') AND a.asset_id=$2 AND ($3::bigint=0 OR v.id<$3)
 ORDER BY v.id DESC LIMIT $4
 ) SELECT n.id,n.kind,n.state,left(`+nodeNavigationDisplaySQL+`,240),v.task_id,v.inherited
 FROM page v `+graphUIPageRowSQL+` WHERE n.kind IN ('intent','fact','finding') ORDER BY n.id DESC`, s.expID, asset, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []AssetRef{}
	for rows.Next() {
		var n AssetRef
		if err = rows.Scan(&n.ID, &n.Kind, &n.State, &n.Summary, &n.SourceTaskID, &n.Inherited); err != nil {
			return nil, false, err
		}
		if !n.Inherited {
			n.SourceTaskID = 0
		}
		out = append(out, n)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
