package db

import (
	"context"
	"encoding/json"
	"fmt"
)

// Navigation is a page-local, disposable index. It does not replace evidence or
// prove that no other records/conflicts exist outside the selected page.
type ContextNavigationRecord struct {
	Ref        string  `json:"ref"`
	AssetIDs   []int64 `json:"asset_ids"`
	AssetsMore bool    `json:"assets_more"`
	Endpoint   string  `json:"endpoint,omitempty"`
	Phase      string  `json:"phase,omitempty"`
	ClaimKey   string  `json:"claim_key,omitempty"`
	Outcome    string  `json:"outcome,omitempty"`
	IntentID   int64   `json:"intent_id,omitempty"`
}

func (s *ExplorationStore) ContextNavigation(ctx context.Context, ids []int64) ([]ContextNavigationRecord, error) {
	if len(ids) > 50 {
		return nil, fmt.Errorf("navigation page exceeds 50 nodes")
	}
	out := []ContextNavigationRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	roots := `SELECT n.id,n.exploration_id,n.state,n.kind,n.created_at,c.owner FROM exploration_nodes n JOIN contexts c ON c.exp=n.exploration_id WHERE n.id=ANY($2::bigint[]) AND (n.exploration_id=$1 OR n.kind<>'intent' OR n.state IN ('done','blocked','exhausted','stopped'))`
	rows, err := s.contextQuery(ctx, contextVisibilityAssets(roots, s.workerRead)+` SELECT 'node:'||n.id,
 to_jsonb(ARRAY(SELECT a.asset_id FROM anchors a WHERE a.root=n.id ORDER BY a.asset_id LIMIT 9)),
 left(COALESCE(CASE WHEN jsonb_typeof(n.payload->'endpoint')='string' THEN n.payload->>'endpoint' END,CASE WHEN jsonb_typeof(n.payload->'url')='string' THEN n.payload->>'url' END,''),200),
 left(COALESCE(CASE WHEN jsonb_typeof(n.payload->'phase')='string' THEN n.payload->>'phase' END,''),80),
 left(COALESCE(CASE WHEN jsonb_typeof(n.payload->'claim_key')='string' THEN n.payload->>'claim_key' END,''),200),
 CASE WHEN length(COALESCE(n.payload->>'endpoint',''))<=200 AND length(COALESCE(n.payload->>'phase',''))<=80 AND length(COALESCE(n.payload->>'claim_key',''))<=200 AND n.kind='fact' AND n.payload->>'outcome' IN ('confirmed','refuted','inconclusive') THEN n.payload->>'outcome' ELSE '' END,
 COALESCE((SELECT e.src_id FROM exploration_edges e JOIN exploration_nodes p ON p.id=e.src_id AND p.kind='intent' WHERE e.exploration_id=n.exploration_id AND e.dst_id=n.id AND e.rel='yields' ORDER BY e.src_id LIMIT 1),0)
 FROM visible v JOIN exploration_nodes n ON n.id=v.id WHERE $3::text='' AND $4::text='' ORDER BY n.id DESC`, s.expID, ids, "", "", int64(0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n ContextNavigationRecord
		var assets []byte
		if err = rows.Scan(&n.Ref, &assets, &n.Endpoint, &n.Phase, &n.ClaimKey, &n.Outcome, &n.IntentID); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(assets, &n.AssetIDs); err != nil {
			return nil, err
		}
		if len(n.AssetIDs) > 8 {
			n.AssetIDs = n.AssetIDs[:8]
			n.AssetsMore = true
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
