package agent

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestBuiltinProtocolMetadataRejectsStaleFieldInstructions(t *testing.T) {
	ts := NewToolSet(nil, "")
	for _, base := range []actool.CoreTool{ts.contextSearch(), ts.contextGet(), ts.assetSearch(), ts.assetGet(), ts.addIntent(), ts.listTaskAssets()} {
		raw, _ := json.Marshal(base.InputSchema())
		var saved map[string]any
		json.Unmarshal(raw, &saved)
		var stale func(map[string]any)
		stale = func(m map[string]any) {
			m["description"] = "旧说明：调用 node_detail，activity 需单独查询"
			for _, v := range m {
				if c, ok := v.(map[string]any); ok {
					stale(c)
				}
			}
		}
		stale(saved)
		resolved := ResolveBuiltinTool(base, "旧说明 node_detail", saved)
		got, _ := json.Marshal(resolved.InputSchema())
		if string(got) != string(raw) || resolved.Description() != base.Description() {
			t.Fatalf("%s: stale metadata changed executable docs: %s", base.Name(), got)
		}
	}
	base := ts.contextSearch()
	tool := ResolveBuiltinTool(base, "", obj(map[string]any{"limit": map[string]any{"description": "上限1000", "default": 20}}))
	raw, err := PrepareToolInput(tool, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(raw), `"limit":20`) || !strings.Contains(tool.Description(), "default 为准") {
		t.Fatal("valid customized defaults lost or contradicted", string(raw), tool.Description(), err)
	}
	// Non-protocol tools remain editable.
	tool = ResolveBuiltinTool(ts.addHint(), "custom instructions", obj(map[string]any{"text": map[string]any{"description": "custom field"}}))
	if tool.Description() != "custom instructions" {
		t.Fatal(tool.Description())
	}
}

func TestActiveBuiltinDocumentationHasNoRetiredToolNames(t *testing.T) {
	check := func(label, text string) {
		t.Helper()
		for old := range db.RetiredToolReplacements {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`).MatchString(text) {
				t.Errorf("%s advertises retired %s", label, old)
			}
		}
	}
	for _, seed := range BuiltinToolSeeds() {
		raw, _ := json.Marshal(seed.Schema)
		check(seed.Key, seed.Desc+string(raw))
	}
	prompts := BuiltinPromptSeeds()
	prompts["reporter"] = ReporterDefaultPrompt
	for role, text := range prompts {
		check(role, text)
	}
	if strings.Contains(autoDefaultTmpl, "update_skill ") || !strings.Contains(autoDefaultTmpl, "update_skill_file") {
		t.Fatal("unknown skill tool")
	}
	if strings.Contains(plannerDefaultTmpl, "上游意图/发现也传") || !strings.Contains(plannerDefaultTmpl, "不能填写意图/目标/提示") {
		t.Fatal("parent ID guidance disagrees with handler")
	}
	if strings.Contains(plannerDefaultTmpl, "资产图全局共享") || !strings.Contains(plannerDefaultTmpl, "全部候选不可用时结束本轮") {
		t.Fatal("planning scope contradicts access policy")
	}
}

func TestIntentBatchDocumentationUsesSingleItemContract(t *testing.T) {
	tool := NewToolSet(nil, "").addIntent()
	props := tool.InputSchema()["properties"].(map[string]any)
	batch := props["intents"].(map[string]any)["items"].(map[string]any)
	for _, key := range []string{"summary", "asset_ids", "parent_ids", "priority"} {
		if !reflect.DeepEqual(props[key], batch["properties"].(map[string]any)[key]) {
			t.Fatal(key)
		}
	}
	// Keep per-item partial failure semantics: no new batch-required-field gate.
	for _, raw := range []string{`{"intents":[{"summary":"one","asset_ids":["123"],"parent_ids":[456]},{}]}`, `{"summary":"one","parent_ids":["456"]}`} {
		if err := validateBusinessInput(normalizeToolSchema(tool.InputSchema()), json.RawMessage(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
}

func TestStoredNestedDefaultsAreNotAdvertisedAsInjected(t *testing.T) {
	tool := NewToolSet(nil, "").contextGet()
	saved := obj(map[string]any{"items": map[string]any{"default": []any{map[string]any{"ref": "node:1"}}, "items": obj(map[string]any{"view": map[string]any{"default": "output"}, "max_chars": map[string]any{"default": 12000}})}})
	resolved := ResolveBuiltinTool(tool, "", saved)
	got, _ := json.Marshal(resolved.InputSchema())
	want, _ := json.Marshal(tool.InputSchema())
	if string(got) != string(want) {
		t.Fatal("uninjected nested defaults advertised", string(got))
	}
}
