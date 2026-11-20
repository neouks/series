//go:build liveeval

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
)

type builtinLiveEvent struct {
	Name     string          `json:"name"`
	Position int             `json:"position"`
	Accepted bool            `json:"accepted"`
	Input    json.RawMessage `json:"input"`
}
type builtinLiveFixture struct {
	mu       sync.Mutex
	steps    []string
	position int
	seed     string
	final    string
	tools    []actool.CoreTool
	events   []builtinLiveEvent
	pressure bool // opt-in long-context fixture; ordinary campaigns are unchanged
}

func newBuiltinLiveFixture(t *testing.T, scenario string, repeat int) *builtinLiveFixture {
	t.Helper()
	f := &builtinLiveFixture{seed: fmt.Sprintf("builtin-v1/%s/%s/%d", os.Getenv("ARTEX_LIVE_CAMPAIGN"), scenario, repeat)}
	switch scenario {
	case "short":
		f.steps = []string{"asset_search", "asset_get", "record_fact"}
	case "gap2":
		f.steps = []string{"asset_get", "context_search", "context_get", "asset_get", "context_search", "context_get", "asset_get", "record_fact"}
	case "gap4":
		f.steps = []string{"asset_get", "context_search", "context_get", "context_search", "context_get", "asset_get", "context_search", "context_get", "context_search", "context_get", "asset_get", "record_fact"}
	case "catalog":
		f.steps = []string{"asset_search", "asset_get", "list_task_assets", "check_target_access", "graph_overview", "context_search", "context_get", "list_finding_deletion_feedback", "add_hint", "asset_get", "asset_search", "record_fact"}
	case "pressure_revisit", "pressure_mixed":
		f.steps, f.pressure = builtinPressureSteps(scenario), true
	default:
		t.Fatalf("unknown scenario %s", scenario)
	}
	f.final = liveReceipt(f.seed, 999)
	// Exact current MainAgent schemas/descriptions, with ONLY execution replaced.
	// The empty stores are never called. No artificial description padding.
	for _, source := range agent.NewToolSet(nil, "").MainAgentTools() {
		name := source.Name()
		tool := actool.Build(actool.Spec{Name: name, Description: source.Description(), Schema: source.InputSchema(),
			ReadOnly: func(json.RawMessage) bool { return true },
			Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.Allowed()
			},
			Run: func(_ context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
				return f.execute(name, in), nil
			},
		})
		f.tools = append(f.tools, agent.DecorateTool(tool, source.Description(), source.InputSchema()))
	}
	return f
}

func (f *builtinLiveFixture) values(position int) (int, int, string, string) {
	token := liveReceipt(f.seed, position)
	n, _ := strconv.ParseInt(token[:6], 16, 64)
	return 10000 + int(n%80000), 100000 + int(n%80000), token, "eval-" + token + ".example.test"
}

func (f *builtinLiveFixture) instruction(position int) string {
	asset, node, token, domain := f.values(position)
	switch f.steps[position] {
	case "asset_search":
		return fmt.Sprintf("精确检索域名 %s 的资产摘要，独立资产类型选择 subdomain，最多3条。", domain)
	case "asset_get":
		return fmt.Sprintf("读取资产编号 %d 的凭证字段组，整批正文预算1200字符。", asset)
	case "context_search":
		return fmt.Sprintf("在本任务中检索资产 %d 的事实与漏洞两类记录，字面关键词 %s，最多3条。", asset, token)
	case "context_get":
		return fmt.Sprintf("读取探索节点 node:%d 的 /evidence 正文，整批正文预算1200字符。", node)
	case "record_fact":
		return fmt.Sprintf("写回一条实测事实，summary 必须为“核验-%s”，evidence 必须为“fixture observed %s”，关联资产 %d、当前意图 %d；置信度按直接观察选择。", token, token, asset, node)
	case "list_task_assets":
		return fmt.Sprintf("读取审批管理中的 approved 资产列表，以主机名称 %s 包含搜索，最多3条，需列表而非仅计数。", domain)
	case "check_target_access":
		return fmt.Sprintf("仅查询未知候选资产 %d 和节点 %d 的审批元数据。", asset, node)
	case "graph_overview":
		return "读取本任务的有界整体态势窗口。"
	case "list_finding_deletion_feedback":
		if f.pressure {
			return fmt.Sprintf("读取本任务用户删除漏洞的原因列表，最多%d条，只取编号小于%d的记录，不指定单条id。", 1+position%3, node)
		}
		return "读取本任务用户删除漏洞的原因列表，最多3条，从第一页开始。"
	case "add_task_scope":
		return fmt.Sprintf("向本任务一次批量加入两条范围：精确子域 %s，以及IP 192.0.2.%d；审计依据必须原样为“测试授权-%s”。", domain, 1+asset%200, token)
	case "add_company_scope":
		return fmt.Sprintf("把两行范围 %s 和 198.51.100.%d 归入公司“隔离公司-%s”，归属依据必须原样为“证书核验-%s”；不设置logo。", domain, 1+asset%200, token, token)
	case "add_hint":
		return fmt.Sprintf("新增一条战略提示，内容必须为“核对-%s”，锚定资产 %d。", token, asset)
	}
	panic("missing instruction")
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (f *builtinLiveFixture) valid(name string, in map[string]any) bool {
	if f.position >= len(f.steps) || name != f.steps[f.position] {
		return false
	}
	asset, node, token, domain := f.values(f.position)
	if v, ok := in["task_id"]; ok && v != "" && v != nil {
		return false
	}
	switch name {
	case "asset_search":
		dsl, _ := in["dsl"].(string)
		dsl = strings.NewReplacer(" ", "", "\"", "", "'", "").Replace(dsl)
		return dsl == "domain=="+domain && in["type"] == "subdomain" && in["limit"] == float64(3)
	case "asset_get":
		groups, _ := in["fields"].([]any)
		return jsonEqual(in["ids"], []int{asset}) && slices.Contains(groups, any("auth")) && in["max_chars"] == float64(1200)
	case "context_search":
		types, _ := in["types"].([]any)
		return len(types) == 2 && slices.Contains(types, any("fact")) && slices.Contains(types, any("finding")) && in["asset_id"] == float64(asset) && in["q"] == token && in["limit"] == float64(3)
	case "context_get":
		items, _ := in["items"].([]any)
		if len(items) != 1 {
			return false
		}
		item, _ := items[0].(map[string]any)
		return item["ref"] == fmt.Sprintf("node:%d", node) && item["field"] == "/evidence" && (item["view"] == nil || item["view"] == "detail") && in["max_chars"] == float64(1200)
	case "record_fact":
		item := in
		if batch, ok := in["facts"].([]any); ok {
			if len(batch) != 1 {
				return false
			}
			item, _ = batch[0].(map[string]any)
		}
		intent := item["intent_id"]
		if intent == nil {
			intent = in["intent_id"]
		}
		return item["summary"] == "核验-"+token && item["evidence"] == "fixture observed "+token && item["confidence"] == "observed" && jsonEqual(item["asset_ids"], []int{asset}) && (intent == float64(node) || intent == strconv.Itoa(node))
	case "list_task_assets":
		return in["status"] == "approved" && in["q"] == domain && in["limit"] == float64(3) && (in["summary_only"] == nil || in["summary_only"] == false)
	case "check_target_access":
		return jsonEqual(in["asset_ids"], []int{asset}) && jsonEqual(in["node_ids"], []int{node})
	case "graph_overview":
		return true
	case "list_finding_deletion_feedback":
		if f.pressure {
			return in["limit"] == float64(1+f.position%3) && in["before"] == float64(node) && in["id"] == nil
		}
		return in["limit"] == float64(3) && (in["before"] == nil || in["before"] == float64(0)) && in["id"] == nil
	case "add_task_scope":
		entries, _ := in["entries"].([]any)
		want := []any{map[string]any{"kind": "subdomain", "value": domain}, map[string]any{"kind": "ip", "value": fmt.Sprintf("192.0.2.%d", 1+asset%200)}}
		return len(entries) == 2 && ((jsonEqual(entries[0], want[0]) && jsonEqual(entries[1], want[1])) || (jsonEqual(entries[0], want[1]) && jsonEqual(entries[1], want[0]))) && in["reason"] == "测试授权-"+token
	case "add_company_scope":
		return in["company"] == "隔离公司-"+token && in["scope"] == fmt.Sprintf("%s\n198.51.100.%d", domain, 1+asset%200) && in["reason"] == "证书核验-"+token && (in["logo"] == nil || in["logo"] == "")
	case "add_hint":
		item := in
		if batch, ok := in["hints"].([]any); ok {
			if len(batch) != 1 {
				return false
			}
			item, _ = batch[0].(map[string]any)
		}
		return item["text"] == "核对-"+token && jsonEqual(item["asset_ids"], []int{asset})
	}
	return false
}

func (f *builtinLiveFixture) execute(name string, raw json.RawMessage) actool.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	var input map[string]any
	ok := json.Unmarshal(raw, &input) == nil && f.valid(name, input)
	f.events = append(f.events, builtinLiveEvent{name, f.position, ok, append(json.RawMessage(nil), raw...)})
	if !ok {
		return actool.Errorf("参数或工具选择不符合本步骤。核对当前 next_instruction 的动态编号、字段组、预算、类型与业务语义；不要跳过步骤。")
	}
	f.position++
	var value any = map[string]any{"done": true, "final_receipt": f.final}
	if f.position < len(f.steps) {
		value = map[string]any{"done": false, "completed_steps": f.position, "next_instruction": f.instruction(f.position)}
		if f.pressure && name == "context_get" {
			value.(map[string]any)["evidence_excerpt"] = builtinPressureExcerpt(f.seed, f.position-1)
		}
	}
	encoded, _ := json.Marshal(value)
	return actool.Text(string(encoded))
}

type builtinLiveMetrics struct {
	Variant           string `json:"variant"`
	Scenario          string `json:"scenario"`
	Repeat            int    `json:"repeat"`
	Passed            bool   `json:"passed"`
	Steps             int    `json:"completed_steps"`
	Required          int    `json:"required_steps"`
	Requests          int    `json:"model_requests"`
	Executions        int    `json:"execution_attempts"`
	BackendRejected   int    `json:"backend_rejected"`
	ParameterErrors   int    `json:"business_parameter_errors"`
	WrongTool         int    `json:"wrong_tool_calls"`
	Discoveries       int    `json:"discovery_calls"`
	RepeatDiscoveries int    `json:"repeated_discoveries"`
	RepeatedTools     int    `json:"repeated_tool_loads"`
	Help              int    `json:"help_calls"`
	SchemaErrors      int    `json:"schema_errors"`
	DeclarationErrors int    `json:"declaration_errors"`
	ToolErrors        int    `json:"tool_errors"`
	Input             int    `json:"input_tokens"`
	Cache             int    `json:"cache_read_tokens"`
	Uncached          int    `json:"uncached_input_tokens"`
	Output            int    `json:"output_tokens"`
	PeakInput         int    `json:"peak_input_tokens"`
	DeclarationBytes  int    `json:"declaration_bytes_sum"`
	PeakTools         int    `json:"peak_active_tools"`
	Milliseconds      int64  `json:"milliseconds"`
	Error             string `json:"error,omitempty"`
}

func measureBuiltinLive(run agent.LiveToolRun, f *builtinLiveFixture, scenario string, repeat int, elapsed time.Duration, format string) builtinLiveMetrics {
	m := builtinLiveMetrics{Variant: run.Variant, Scenario: scenario, Repeat: repeat, Steps: f.position, Required: len(f.steps), Requests: len(run.Requests), Input: run.Usage.InputTokens, Cache: run.Usage.CacheReadTokens, Output: run.Usage.OutputTokens, Milliseconds: elapsed.Milliseconds(), Error: run.Error}
	if format == "anthropic" {
		m.Input += run.Usage.CacheReadTokens + run.Usage.CacheWriteTokens
	}
	m.Uncached = m.Input - m.Cache
	m.Passed = f.position == len(f.steps) && strings.TrimSpace(run.Final) == f.final
	for _, e := range f.events {
		if !e.Accepted {
			m.BackendRejected++
			if e.Position < len(f.steps) && e.Name == f.steps[e.Position] {
				m.ParameterErrors++
			} else {
				m.WrongTool++
			}
		}
	}
	calls := map[string]string{}
	seen := map[string]bool{}
	for _, message := range run.Messages {
		for _, b := range message.Content {
			if b.Type == llm.BlockToolUse {
				name := b.Name
				if name == actool.ExecuteExtraToolName {
					var in struct {
						Name string `json:"tool_name"`
					}
					json.Unmarshal(b.Input, &in)
					name = in.Name
				}
				calls[b.ID] = name
				switch name {
				case agent.BuiltinSearchName, actool.SearchExtraToolsName:
					m.Discoveries++
				case "GetToolHelp":
					m.Help++
				default:
					m.Executions++
				}
			}
			if b.Type == llm.BlockToolResult {
				body := ""
				for _, p := range b.Content {
					body += p.Text
				}
				if b.IsError {
					m.ToolErrors++
				}
				if strings.Contains(body, "invalid tool input") || strings.Contains(body, "参数错误:") {
					m.SchemaErrors++
				}
				if strings.Contains(body, "尚未加载") || strings.Contains(body, "tool schema is absent") || strings.Contains(body, "not available in the current execution phase") {
					m.DeclarationErrors++
				}
				loaded := []string{}
				if !b.IsError && calls[b.ToolUseID] == agent.BuiltinSearchName {
					var r struct {
						Loaded  []string `json:"loaded"`
						Already []string `json:"already_loaded"`
					}
					json.Unmarshal([]byte(body), &r)
					loaded = append(r.Loaded, r.Already...)
				}
				if !b.IsError && calls[b.ToolUseID] == actool.SearchExtraToolsName && strings.HasPrefix(body, "Found ") {
					for _, p := range strings.Split(body, "\n\n## ")[1:] {
						n, _, _ := strings.Cut(p, "\n")
						loaded = append(loaded, n)
					}
				}
				repeated := false
				for _, n := range loaded {
					if seen[n] {
						repeated = true
						m.RepeatedTools++
					}
					seen[n] = true
				}
				if repeated {
					m.RepeatDiscoveries++
				}
			}
		}
	}
	for _, r := range run.Requests {
		m.PeakInput = max(m.PeakInput, r.Usage.InputTokens)
		raw, _ := json.Marshal(r.Request.Tools)
		m.DeclarationBytes += len(raw)
		m.PeakTools = max(m.PeakTools, len(r.Request.Tools))
	}
	return m
}

func TestLiveBuiltinFixtureProtocol(t *testing.T) {
	for _, scenario := range []string{"short", "gap2", "gap4", "catalog", "pressure_revisit", "pressure_mixed"} {
		f := newBuiltinLiveFixture(t, scenario, 0)
		for pos, name := range f.steps {
			a, n, tok, d := f.values(pos)
			var in map[string]any
			switch name {
			case "asset_search":
				in = map[string]any{"dsl": "domain==" + d, "type": "subdomain", "limit": 3}
			case "asset_get":
				in = map[string]any{"ids": []int{a}, "fields": []string{"auth"}, "max_chars": 1200}
			case "context_search":
				in = map[string]any{"types": []string{"fact", "finding"}, "asset_id": a, "q": tok, "limit": 3}
			case "context_get":
				in = map[string]any{"items": []any{map[string]any{"ref": fmt.Sprintf("node:%d", n), "field": "/evidence", "view": "detail"}}, "max_chars": 1200}
			case "record_fact":
				in = map[string]any{"summary": "核验-" + tok, "evidence": "fixture observed " + tok, "confidence": "observed", "asset_ids": []int{a}, "intent_id": n}
			case "list_task_assets":
				in = map[string]any{"status": "approved", "q": d, "limit": 3}
			case "check_target_access":
				in = map[string]any{"asset_ids": []int{a}, "node_ids": []int{n}}
			case "graph_overview":
				in = map[string]any{}
			case "list_finding_deletion_feedback":
				in = map[string]any{"limit": 3}
				if f.pressure {
					in = map[string]any{"limit": 1 + pos%3, "before": n}
				}
			case "add_task_scope":
				in = map[string]any{"entries": []any{map[string]any{"kind": "subdomain", "value": d}, map[string]any{"kind": "ip", "value": fmt.Sprintf("192.0.2.%d", 1+a%200)}}, "reason": "测试授权-" + tok}
			case "add_company_scope":
				in = map[string]any{"company": "隔离公司-" + tok, "scope": fmt.Sprintf("%s\n198.51.100.%d", d, 1+a%200), "reason": "证书核验-" + tok}
			case "add_hint":
				in = map[string]any{"hints": []any{map[string]any{"text": "核对-" + tok, "asset_ids": []int{a}}}}
			}
			tool, _ := actool.NewRegistry(f.tools...).Get(name)
			raw, _ := json.Marshal(in)
			prepared, err := agent.PrepareToolInput(tool, raw)
			if err != nil {
				t.Fatal(name, err)
			}
			if err := actool.ValidateInput(tool.InputSchema(), prepared); err != nil {
				t.Fatal(name, err)
			}
			result, err := tool.Call(t.Context(), raw, nil)
			if err != nil || result.IsError {
				t.Fatal(name, err, result.Flatten())
			}
		}
		if f.position != len(f.steps) {
			t.Fatal("incomplete fixture")
		}
	}
}

func TestLiveBuiltinComparison(t *testing.T) {
	settings := loadLiveSettings(t)
	// Pin the prior campaign identity rather than silently test a new default.
	if settings.Model != "deepseek-flash" || settings.Format != "openai" || settings.ReasoningEffort != "high" || settings.ThinkingType != "enabled" {
		t.Fatal("production profile differs from the previous campaign; stop for review")
	}
	if settings.BaseURL == "" || settings.APIKey == "" {
		t.Fatal("model connection is incomplete")
	}
	cfg := agent.ConfigFrom(settings.Format, settings.Model, settings.BaseURL, settings.APIKey, settings.Proxy)
	cfg.ReasoningEffort, cfg.ThinkingType, cfg.MaxTokensField = settings.ReasoningEffort, settings.ThinkingType, settings.MaxTokensField
	cfg.SessionHeaderKey, cfg.ContextWindowK = settings.SessionHeaderKey, settings.ContextWindowK
	cfg.Retry = agent.RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1, StreamAttempts: -1}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("ARTEX_LIVE_OUT")
	if out == "" {
		t.Fatal("ARTEX_LIVE_OUT required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "summary.json")); err == nil {
		t.Fatal("use a fresh output directory")
	}
	repeats := 3
	if v := os.Getenv("ARTEX_LIVE_REPEATS"); v != "" {
		repeats, err = strconv.Atoi(v)
		if err != nil || repeats < 1 || repeats > 5 {
			t.Fatal("repeats must be 1..5")
		}
	}
	scenarios := []string{"short", "gap2", "gap4", "catalog"}
	if s := os.Getenv("ARTEX_LIVE_SCENARIO"); s != "" {
		scenarios = []string{s}
	}
	variants := []string{"before", "stable"}
	if s := os.Getenv("ARTEX_LIVE_VARIANTS"); s != "" {
		variants = strings.Split(s, ",")
	}
	maxTokens := settings.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	window, maxTurns := cfg.CompactionWindow(), 50
	if v := os.Getenv("ARTEX_LIVE_PRESSURE_WINDOW"); v != "" {
		window, err = strconv.Atoi(v)
		if err != nil || window < 16000 || window > cfg.CompactionWindow() {
			t.Fatal("invalid isolated pressure window")
		}
		for _, s := range scenarios {
			if !strings.HasPrefix(s, "pressure_") {
				t.Fatal("pressure window requires pressure fixture")
			}
		}
		maxTurns = 120
	}
	var rows []builtinLiveMetrics
	repeatStart := 0
	if v := os.Getenv("ARTEX_LIVE_REPEAT_START"); v != "" {
		repeatStart, err = strconv.Atoi(v)
		if err != nil || repeatStart < 0 || repeatStart > 20 {
			t.Fatal("repeat start must be 0..20")
		}
	}
	for repeat := repeatStart; repeat < repeatStart+repeats; repeat++ {
		for si, scenario := range scenarios {
			for offset := 0; offset < len(variants); offset++ {
				variant := variants[(offset+si+repeat)%len(variants)]
				name := fmt.Sprintf("%s-%s-r%d", scenario, variant, repeat+1)
				t.Run(name, func(t *testing.T) {
					f := newBuiltinLiveFixture(t, scenario, repeat)
					system := []string{"你是工具调用评测助手，完成隔离的合成任务。所有提供工具均已获测试授权；按 next_instruction 逐步执行，不跳步、不预猜后续数据、不用历史结果代替新的调用。工具不可见时，若提供 tool_search，用 exact/keyword/catalog/auto 发现内置能力并在下一轮读取完整声明后直接调用；若提供 SearchExtraTools，按关键词或 select:名称发现延迟工具，再用 ExecuteExtraTool；如需常驻工具完整说明可用 GetToolHelp。以本轮参数契约为准，已有定义够用时直接复用。允许根据错误自行修正。本任务内的读写全部是测试夹具，不需询问用户。done=true 时最终只输出 final_receipt 的实际值（不是字段名）。"}
					prompt := "本任务逐步核验并写回。当前步骤：" + f.instruction(0) + "后续按工具返回的 next_instruction 继续，直到 done=true。"
					opts := agentcore.Options{Provider: provider, Tools: f.tools, SystemPrompt: system, DynamicBoundary: len(system), WorkingDir: t.TempDir(), PermissionMode: permission.ModeBypass, Compaction: &compaction.Config{ContextWindow: window}, MaxTurns: maxTurns, MaxTokens: maxTokens, SessionID: "builtin-eval-" + name}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
					defer cancel()
					start := time.Now()
					run, err := agent.RunLiveBuiltinEvaluation(ctx, opts, prompt, variant, func(r agent.LiveToolRequest) {
						t.Logf("REQUEST %s #%d input=%d cache=%d output=%d error=%v", name, r.Turn, r.Usage.InputTokens, r.Usage.CacheReadTokens, r.Usage.OutputTokens, r.Error != "")
						raw, _ := json.Marshal(r)
						file, e := os.OpenFile(filepath.Join(out, name+".requests.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
						if e != nil {
							t.Error(e)
							return
						}
						_, e = file.Write(append(raw, '\n'))
						file.Close()
						if e != nil {
							t.Error(e)
						}
					})
					if err != nil {
						t.Fatal(err)
					}
					row := measureBuiltinLive(run, f, scenario, repeat+1, time.Since(start), settings.Format)
					rows = append(rows, row)
					artifacts := map[string]any{name + ".json": run, name + ".backend.json": f.events, "summary.json": map[string]any{"model": settings.Model, "format": settings.Format, "reasoning_effort": settings.ReasoningEffort, "thinking_type": settings.ThinkingType, "context_window": window, "production_context_window": cfg.CompactionWindow(), "max_tokens": maxTokens, "max_turns": maxTurns, "transport": "non-streaming", "repeats": repeats, "campaign": os.Getenv("ARTEX_LIVE_CAMPAIGN"), "metrics": rows}}
					for file, value := range artifacts {
						raw, _ := json.MarshalIndent(value, "", "  ")
						if err := os.WriteFile(filepath.Join(out, file), raw, 0600); err != nil {
							t.Fatal(err)
						}
					}
					t.Logf("RESULT %s passed=%v steps=%d/%d attempts=%d requests=%d discovery=%d repeat=%d uncached=%d", name, row.Passed, row.Steps, row.Required, row.Executions, row.Requests, row.Discoveries, row.RepeatDiscoveries, row.Uncached)
					if row.Error != "" && row.Input == 0 {
						t.Errorf("infrastructure failure: %s", row.Error)
					}
				})
				if t.Failed() {
					return
				}
			}
		}
	}
}

func TestLiveBuiltinMetricsCountDiscoveries(t *testing.T) {
	f := newBuiltinLiveFixture(t, "short", 0)
	r := agent.LiveToolRun{Variant: "idle2", Messages: []llm.Message{}}
	for i := 0; i < 2; i++ {
		id := fmt.Sprint(i)
		r.Messages = append(r.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: agent.BuiltinSearchName, ID: id, Input: json.RawMessage(`{"mode":"exact","query":"asset_get"}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, `{"loaded":["asset_get"],"already_loaded":[]}`, false)}})
	}
	m := measureBuiltinLive(r, f, "short", 1, 0, "openai")
	if !reflect.DeepEqual([]int{m.Discoveries, m.RepeatDiscoveries, m.RepeatedTools, m.Executions}, []int{2, 1, 1, 0}) {
		t.Fatalf("bad metrics: %+v", m)
	}
}
