package agent

import (
	"reflect"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestAssetGetResidentAndTargetAccessOnDemand(t *testing.T) {
	if !coreTool("asset_get", "worker") || !BuiltinResident("worker", "asset_get") {
		t.Fatal("asset_get must be resident")
	}
	if coreTool("check_target_access", "worker") || BuiltinResident("worker", "check_target_access") {
		t.Fatal("check_target_access must remain on demand")
	}
	p := (&builtinCatalog{options: BuiltinLoadingOptions{Role: "worker", Mode: "on_demand", SearchAllowed: true, Known: map[string]bool{BuiltinSearchName: true, "asset_get": true, "check_target_access": true}}}).fresh()
	req := llm.CompletionRequest{Tools: []llm.ToolSchema{{Name: BuiltinSearchName}, {Name: "asset_get", InputSchema: obj(map[string]any{"ids": map[string]any{"type": "array"}})}, {Name: "check_target_access", InputSchema: obj(nil)}}}
	out, err := p.project(t.Context(), req, false)
	if err != nil || !hasBuiltin(out, "asset_get") || hasBuiltin(out, "check_target_access") || !hasBuiltin(out, BuiltinSearchName) {
		t.Fatalf("incorrect initial declarations: %v", err)
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "check_target_access"}); !reflect.DeepEqual(got.Loaded, []string{"check_target_access"}) {
		t.Fatalf("could not load check_target_access: %+v", got)
	}
	out, err = p.project(t.Context(), req, false)
	if err != nil || !hasBuiltin(out, "asset_get") || !hasBuiltin(out, "check_target_access") || !reflect.DeepEqual(out.Tools[1].InputSchema, req.Tools[1].InputSchema) {
		t.Fatalf("loaded declarations lost: %v", err)
	}
	direct := p.fresh()
	direct.options.Mode = "direct"
	out, err = direct.project(t.Context(), req, false)
	if err != nil || !hasBuiltin(out, "asset_get") || !hasBuiltin(out, "check_target_access") {
		t.Fatalf("direct mode lost tools: %v", err)
	}
}
