package agent

import (
	"context"
	"encoding/json"
	"fmt"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func (t *ToolSet) contextNavigation(ctx context.Context, a contextSearchInput, hits []db.ContextHit, cross bool) map[string]any {
	ids := []int64{}
	reads := map[string]map[string]any{}
	for _, h := range hits {
		if h.Kind != "activity" {
			ids = append(ids, h.NodeID)
			if h.Read != nil {
				reads[h.Ref] = h.Read
			}
		}
	}
	rows, err := t.ts.ContextNavigation(ctx, ids)
	if err != nil {
		return map[string]any{"scope": "current_search_page", "unavailable": "导航查询失败；原检索结果仍有效"}
	}
	return navigationDirectory(rows, reads)
}
func navigationDirectory(rows []db.ContextNavigationRecord, reads map[string]map[string]any) map[string]any {
	groups := map[string][]string{}
	incomplete := false
	missing := map[string]int{"asset": 0, "endpoint": 0, "phase": 0}
	claims := map[string][]db.ContextNavigationRecord{}
	for _, r := range rows {
		incomplete = incomplete || r.AssetsMore
		if len(r.AssetIDs) == 0 {
			missing["asset"]++
		}
		if r.Endpoint == "" {
			missing["endpoint"]++
		}
		if r.Phase == "" {
			missing["phase"]++
		}
		for _, id := range r.AssetIDs {
			groups[fmt.Sprintf("asset:%d", id)] = append(groups[fmt.Sprintf("asset:%d", id)], r.Ref)
		}
		if r.Endpoint != "" {
			label := navigationEndpoint(r.Endpoint)
			groups["endpoint:"+label] = append(groups["endpoint:"+label], r.Ref)
		}
		if r.Phase != "" {
			groups["phase:"+r.Phase] = append(groups["phase:"+r.Phase], r.Ref)
		}
		// No NLP inference: only explicit same claim + same declared context. An
		// incomplete asset set or absent scope cannot support conflict matching.
		if r.ClaimKey != "" && r.Outcome != "" && !r.AssetsMore && (len(r.AssetIDs) > 0 || r.IntentID > 0) {
			scope := fmt.Sprint(r.AssetIDs)
			if len(r.AssetIDs) == 0 {
				scope = fmt.Sprint("intent:", r.IntentID)
			}
			key := scope + "\x00" + r.Endpoint + "\x00" + r.Phase + "\x00" + r.ClaimKey
			claims[key] = append(claims[key], r)
		}
	}
	keys := []string{}
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	entries := []any{}
	conflicts := []any{}
	remaining := 2000
	omitted := false
	add := func(dst *[]any, row any) {
		raw, _ := json.Marshal(row)
		cost := utf8.RuneCount(raw) + 1
		if cost > remaining {
			omitted = true
			return
		}
		remaining -= cost
		*dst = append(*dst, row)
	}
	claimKeys := []string{}
	for k := range claims {
		claimKeys = append(claimKeys, k)
	}
	sort.Strings(claimKeys)
	for _, key := range claimKeys {
		items := claims[key]
		positive, negative := false, false
		for _, r := range items {
			positive = positive || r.Outcome == "confirmed"
			negative = negative || r.Outcome == "refuted"
		}
		if positive && negative {
			refs := []any{}
			selectedOutcomes := map[string]bool{}
			for _, r := range items {
				if r.Outcome == "inconclusive" || selectedOutcomes[r.Outcome] {
					continue
				}
				selectedOutcomes[r.Outcome] = true
				_, id, _ := strings.Cut(r.Ref, ":")
				node, _ := strconv.ParseInt(id, 10, 64)
				item := map[string]any{"ref": r.Ref, "node_id": node, "outcome": r.Outcome}
				if read := reads[r.Ref]; read != nil {
					item["read"] = read
				}
				refs = append(refs, item)
			}
			nodeIDs := []int64{}
			for _, v := range refs {
				if row, ok := v.(map[string]any); ok {
					if id, ok := row["node_id"].(int64); ok {
						nodeIDs = append(nodeIDs, id)
					}
				}
			}

			add(&conflicts, map[string]any{"node_ids": nodeIDs, "status": "possible_conflict", "representative_pair": true, "more_refs": len(items) > len(refs), "claim_key": items[0].ClaimKey, "items": refs})
		}
	}
	for _, k := range keys {
		kind, value, _ := strings.Cut(k, ":")
		items := []any{}
		for _, ref := range groups[k] {
			_, id, _ := strings.Cut(ref, ":")
			node, _ := strconv.ParseInt(id, 10, 64)
			row := map[string]any{"ref": ref, "node_id": node}
			if read := reads[ref]; read != nil {
				row["read"] = read
			}
			items = append(items, row)
		}
		nodeIDs := []int64{}
		for _, ref := range groups[k] {
			_, id, _ := strings.Cut(ref, ":")
			n, _ := strconv.ParseInt(id, 10, 64)
			nodeIDs = append(nodeIDs, n)
		}
		var identity any = value
		filter := map[string]any{"q": value}
		if kind == "asset" {
			id, _ := strconv.ParseInt(value, 10, 64)
			identity = id
			filter = map[string]any{"asset_id": id}
		}
		add(&entries, map[string]any{"node_ids": nodeIDs, "by": kind, "value": identity, "filter": filter, "items": items})
	}
	return map[string]any{"metadata_incomplete": incomplete, "missing_field_counts": missing, "scope": "current_search_page", "groups": entries, "possible_conflicts": conflicts, "omitted": omitted, "note": "仅当前返回页的显式字段导航；未标记不代表没有矛盾。冲突待回读原文核对，不自动认定哪条正确；摘要、推断和经验不是验证证据。"}
}

// Keep credentials/query values out of a navigation label. The original
// endpoint remains in the fact and is only read via its authorized reference.
func navigationEndpoint(raw string) string {
	prefix := ""
	parts := strings.SplitN(raw, " ", 2)
	if len(parts) == 2 {
		switch parts[0] {
		case "GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH":
			prefix = parts[0] + " "
			raw = parts[1]
		}
	}
	u, e := url.Parse(raw)
	if e != nil {
		return prefix + strings.SplitN(strings.SplitN(raw, "?", 2)[0], "#", 2)[0]
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.ForceQuery = false
	return prefix + u.String()
}

// Optional navigation cannot turn a valid bounded search page into an output
// error. Omit only this derived directory, never hits, errors or the cursor.
func contextSearchResult(ctx context.Context, out map[string]any) (actool.Result, error) {
	if out["navigation"] != nil {
		raw, _ := json.Marshal(out)
		if len(raw) > readOutputBudget(ctx) {
			out["navigation"] = map[string]any{"scope": "current_search_page", "omitted": true, "note": "导航超出本次输出预算，原结果和续页保留；可缩小limit再查看导航。"}
		}
	}
	return contextJSON(ctx, out)
}
