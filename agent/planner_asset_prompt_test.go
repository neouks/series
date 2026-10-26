package agent

import (
	"encoding/json"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestPlannerAssetPromptUsesActualBindings(t *testing.T) {
	ts := NewToolSet(nil, "planner")
	for _, tools := range [][]actool.CoreTool{nil, {ts.assetSearch()}, {ts.assetSearch(), ts.assetGet()}} {
		guidance := plannerAssetGuidance(tools)
		system, _ := deferredSystem("saved custom prompt", DeferredInfo{AssetGuidance: guidance})
		text := strings.Join(system, "\n")
		if !strings.HasPrefix(text, "saved custom prompt") || !strings.HasSuffix(text, guidance) {
			t.Fatal("lost custom prompt or fixed guidance")
		}
		if (len(tools) > 0) != strings.Contains(text, "本轮已提供 asset_search") || (len(tools) < 2) != strings.Contains(text, "本轮未提供 asset_get") {
			t.Fatal(text)
		}
		if !strings.Contains(text, "未审批、封禁、撤回或越界") {
			t.Fatal("approval boundary missing", text)
		}
	}
}

func TestPlannerAssetGuidanceRespectsCoverageOnlySchema(t *testing.T) {
	base := NewToolSet(nil, "planner").assetSearch()
	for _, required := range []any{[]string{"untested"}, []any{"untested"}} {
		schema := deepCopyJSONMap(base.InputSchema())
		schema["required"] = required
		tool := DecorateTool(base, "only untested assets", schema)
		guidance := plannerAssetGuidance([]actool.CoreTool{tool})
		if !strings.Contains(guidance, "传 untested=true") || strings.Contains(guidance, "普通检索传非空") {
			t.Fatal(guidance)
		}
	}
	// Coverage-disabled roles must not receive an unusable required selector.
	ts := NewToolSet(nil, "planner")
	ts.SetCoverageEnabled(false)
	schema := deepCopyJSONMap(base.InputSchema())
	schema["required"] = []any{"untested"}
	tools := ts.StripCoverageParams([]actool.CoreTool{DecorateTool(base, "only untested", schema)})
	if len(tools) != 0 {
		raw, _ := json.Marshal(tools[0].InputSchema())
		t.Fatal(string(raw))
	}
}
