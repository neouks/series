package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func (t *ToolSet) searchMixedContext(ctx context.Context, a contextSearchInput, signature string, cross bool) (actool.Result, error) {
	var envelope struct {
		Signature string                 `json:"s"`
		Position  *db.ContextMixedCursor `json:"m"`
	}
	if a.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(a.Cursor)
		if err != nil || json.Unmarshal(b, &envelope) != nil || envelope.Signature != signature || envelope.Position == nil || !envelope.Position.Valid() {
			return actool.Errorf("游标无效或查询条件已改变"), nil
		}
	}
	hits, next, err := t.ts.SearchMixedContext(ctx, db.ContextSearch{Types: a.Types, Query: a.Q, State: a.State, Severity: a.Severity, AssetID: a.AssetID, IntentID: a.IntentID, Limit: a.Limit}, envelope.Position)
	if err != nil {
		return actool.Errorf("查询失败: " + err.Error()), nil
	}
	more := len(hits) > a.Limit
	if more {
		hits = hits[:a.Limit]
	}
	t.addContextHitReads(ctx, a, hits, cross)
	out := map[string]any{"items": hits, "has_more": more}
	if more {
		envelope.Signature, envelope.Position = signature, next
		b, err := json.Marshal(envelope)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		out["next_cursor"] = base64.RawURLEncoding.EncodeToString(b)
	}
	result, err := contextJSON(ctx, out)
	if err == nil && !result.IsError && ctx.Err() == nil {
		t.recordPlannerSearch(ctx, a, signature, out)
	}
	return result, err
}
