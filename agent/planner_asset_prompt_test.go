package agent

import (
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
