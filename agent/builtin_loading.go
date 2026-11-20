package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	actool "github.com/Autumn-27/norma/tool"
)

const BuiltinSearchName = "tool_search"
const BuiltinLoadingSetting = "builtin_tool_loading"
const builtinLoadingRule = "\n内置工具采用按需声明：tool_search 支持 exact/keyword/catalog/auto；关键词默认只加载一个工具，目录不加载。加载成功后在下一轮工具声明中读取完整参数并直接调用原工具名，不使用 ExecuteExtraTool。高频工具常驻；其余加载后稳定保留，只有按需定义自身超过预算时才淘汰较久未使用的声明，不因正文增长反复卸载。收到声明退出提示后，先精确搜索、下一轮读取完整参数再调用；历史调用、Skill 或提示词不能授予权限。自定义与 MCP 工具仍使用 SearchExtraTools/ExecuteExtraTool。"

// Check is a live authorization/readiness check, not an implementation lookup.
// Empty status means authorized; other statuses are unauthorized/unavailable.
// Known contains catalog identities, including unavailable/unbound tools, but
// only assembled implementations may enter the model's declarations.
type BuiltinLoadingOptions struct {
	Role          string
	Mode          string
	SearchAllowed bool
	Known         map[string]bool
	Check         func(context.Context, string) (string, error)
}

func BuiltinResident(role, name string) bool {
	if coreTool(name, role) {
		return true
	}
	switch name {
	case BuiltinSearchName, "Read", "Bash", "Write", "Edit", "MultiEdit", "context_search", "context_get", "ask_user", "AskUserQuestion":
		return true
	}
	switch role {
	case "planner":
		return name == "graph_overview"
	case "goals":
		return slices.Contains([]string{"set_goals", "set_constraints", "register_user_target"}, name)
	default:
		return name == "record_fact"
	}
}

// This ceiling applies even when a database row or an old prompt grants more.
func BuiltinRoleAllows(role, name string) bool {
	if role != "planner" {
		return true
	}
	if strings.HasPrefix(name, "mcp__") {
		return true
	}
	return slices.Contains([]string{BuiltinSearchName, toolHelpName, settlementMarkerName, "Compress", "Skill", "Read", "LS", "Glob", "Grep", "TodoWrite", "ask_user", "AskUserQuestion", "web_search", "web_fetch", "web_read", "WebFetch",
		"context_search", "context_get", "graph_overview", "asset_search", "asset_get", "list_task_assets", "list_companies", "check_target_access", "traffic_search", "traffic_get", "list_finding_deletion_feedback",
		"add_intent", "prove_goal", "kill_work", "steer_work", "add_task_scope", "report_finding", "add_hint", "add_company_scope", "SearchExtraTools", "ExecuteExtraTool"}, name)
}

func BuiltinToolNames() map[string]bool {
	names := map[string]bool{}
	for _, seed := range BuiltinToolSeeds() {
		names[seed.Key] = true
	}
	for _, name := range []string{BuiltinSearchName, "Read", "Bash", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "TodoWrite", "AskUserQuestion", "ask_user", "web_search", "web_fetch", "WebFetch", "web_read", "http_request", "traffic_search", "traffic_get", "shell_open", "shell_send", "shell_read", "shell_close", "shell_list"} {
		names[name] = true
	}
	return names
}

type builtinCatalog struct {
	mu               sync.Mutex
	options          BuiltinLoadingOptions
	schemas          map[string]llm.ToolSchema
	active           map[string]int
	visible          map[string]bool
	eligible         map[string]bool
	request          int
	window           int // actual configured context window, shared with external schema pressure
	declarationOrder []string
	evicted          map[string]bool // request-tail notice, never an authorization grant
	restored         bool
	settling         bool
}
type builtinSessionKey struct{}
type builtinDeclarationTool struct {
	actool.CoreTool
	catalog *builtinCatalog
}

func (t *builtinDeclarationTool) Description() string {
	text := t.CoreTool.Description()
	if prompt := t.CoreTool.Prompt(); prompt != "" {
		text += "\n\n" + prompt
	}
	return compactToolGuidance(text)
}
func (*builtinDeclarationTool) Prompt() string { return "" }
func (t *builtinDeclarationTool) PrepareInput(raw json.RawMessage) (json.RawMessage, error) {
	return PrepareToolInput(t.CoreTool, raw)
}
func (t *builtinDeclarationTool) AcceptsRawInput() bool {
	v, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && v.AcceptsRawInput()
}
func (t *builtinDeclarationTool) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	p := t.catalog
	if session, ok := ctx.Value(builtinSessionKey{}).(*builtinCatalog); ok {
		p = session
	}
	if status, err := p.check(ctx, t.Name()); err != nil || status != "" {
		return actool.Errorf(fmt.Sprintf("内置工具当前不可用（%s）：%v", status, err)), nil
	}
	// The host hook already checks declaration visibility; this live check is
	// repeated at the executor boundary, after approvals and queued scheduling.
	return t.CoreTool.Call(ctx, raw, tc)
}

func builtinPolicy(t actool.CoreTool) *builtinCatalog {
	switch v := t.(type) {
	case *builtinDeclarationTool:
		return v.catalog
	case *guardedTool:
		return builtinPolicy(v.CoreTool)
	case *discoveredTool:
		return builtinPolicy(v.CoreTool)
	case *residentTool:
		return builtinPolicy(v.CoreTool)
	case *overriddenTool:
		return builtinPolicy(v.CoreTool)
	case *processContextTool:
		return builtinPolicy(v.CoreTool)
	case *interactiveTool:
		return builtinPolicy(v.CoreTool)
	}
	return nil
}

// ConfigureBuiltinTools must run AFTER role authorization and implementation
// construction. A catalog entry alone is never an executable capability.
func ConfigureBuiltinTools(tools []actool.CoreTool, options BuiltinLoadingOptions) []actool.CoreTool {
	if len(tools) == 0 {
		return tools
	}
	for _, t := range tools {
		if builtinPolicy(t) != nil {
			return tools
		}
	}
	if options.Known == nil {
		options.Known = BuiltinToolNames()
	}
	if !options.SearchAllowed || options.Mode == "direct" {
		options.Mode = "direct"
	} else {
		options.Mode = "on_demand"
	}
	p := &builtinCatalog{options: options, schemas: map[string]llm.ToolSchema{}}
	out := make([]actool.CoreTool, 0, len(tools)+1)
	for _, t := range tools {
		if !BuiltinRoleAllows(options.Role, t.Name()) {
			continue
		}
		if t.Name() != BuiltinSearchName {
			t = &builtinDeclarationTool{CoreTool: t, catalog: p}
		}
		if t.Name() != BuiltinSearchName {
			out = append(out, t)
		}
	}
	if options.SearchAllowed {
		out = append(out, &builtinDeclarationTool{CoreTool: newBuiltinSearch(p), catalog: p})
	}
	return out
}

func (p *builtinCatalog) fresh() *builtinCatalog {
	return &builtinCatalog{options: p.options, window: p.window, schemas: map[string]llm.ToolSchema{}, active: map[string]int{}, visible: map[string]bool{}, eligible: map[string]bool{}}
}

func (p *builtinCatalog) check(ctx context.Context, name string) (string, error) {
	if !BuiltinRoleAllows(p.options.Role, name) {
		return "unauthorized", nil
	}
	if p.options.Check != nil {
		return p.options.Check(ctx, name)
	}
	return "", ctx.Err()
}

func (p *builtinCatalog) project(ctx context.Context, req llm.CompletionRequest, settling bool) (llm.CompletionRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.restored {
		p.restore(req.Messages)
		p.restored = true
	}
	p.request++
	p.settling = settling
	p.eligible = map[string]bool{}
	p.visible = map[string]bool{}
	out := make([]llm.ToolSchema, 0, len(req.Tools))
	for _, schema := range req.Tools {
		name := schema.Name
		if !BuiltinRoleAllows(p.options.Role, name) {
			continue
		}
		if !p.options.Known[name] {
			out = append(out, schema)
			continue
		}
		p.schemas[name] = schema
		status, err := p.check(ctx, name)
		if err != nil {
			return req, fmt.Errorf("读取内置工具授权失败: %w", err)
		}
		if status != "" {
			delete(p.active, name)
			continue
		}
		p.eligible[name] = true
		_, loaded := p.active[name]
		keep := p.options.Mode == "direct" || BuiltinResident(p.options.Role, name) || (settling && builtinSettlementTool(name)) || loaded
		if keep {
			out = append(out, schema)
			p.visible[name] = true
		} else {
			delete(p.active, name)
		}
	}
	req.Tools = p.orderDeclarations(out)
	req = p.describeDirectory(req)
	// Legacy successful discoveries may contain complete native definitions.
	// Their raw records remain intact; the live declarations own native contracts.
	req = p.removeHistoricalDefinitions(req)
	return req, nil
}

func (p *builtinCatalog) used(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.eligible[name] {
		p.active[name] = p.request
	}
}

func (p *builtinCatalog) restore(messages []llm.Message) {
	// Only paired successful tool results restore recency. A failed/guessed call,
	// prompt mention, catalog page or arbitrary user JSON never activates a tool.
	age := 0
	type result struct {
		text string
		ok   bool
	}
	results := map[string]result{}
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == llm.RoleUser {
			for j := len(m.Content) - 1; j >= 0; j-- {
				b := m.Content[j]
				if b.Type != llm.BlockToolResult {
					clear(results)
					continue
				}
				if b.ToolUseID == "" {
					continue
				}
				_, duplicate := results[b.ToolUseID]
				valid := !duplicate && !b.IsError && len(b.Content) == 1 && b.Content[0].Type == llm.BlockText
				r := result{ok: valid}
				if valid {
					r.text = b.Content[0].Text
				}
				results[b.ToolUseID] = r
			}
			continue
		}
		if m.Role != llm.RoleAssistant {
			clear(results)
			continue
		}
		ids := map[string]int{}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				ids[b.ID]++
			}
		}
		for _, b := range m.Content {
			if b.Type != llm.BlockToolUse || b.ID == "" || ids[b.ID] != 1 {
				continue
			}
			r, ok := results[b.ID]
			if !ok || !r.ok {
				continue
			}
			names := []string{assetContextCall(b).Name}
			if b.Name == actool.SearchExtraToolsName {
				names = nil
				if cs, ok := retainedContracts(r.text); ok {
					for _, c := range cs {
						names = append(names, c.Name)
					}
				}
			}
			if b.Name == BuiltinSearchName {
				var receipt builtinDiscoveryResult
				if json.Unmarshal([]byte(noaadapter.StripRefTag(r.text)), &receipt) == nil && receipt.Version == 1 && (receipt.Status == "loaded" || receipt.Status == "already_loaded") {
					names = append(receipt.Loaded, receipt.AlreadyLoaded...)
				} else {
					names = nil
				}
			}
			for _, name := range names {
				if p.options.Known[name] {
					if previous, ok := p.active[name]; !ok || previous < -age {
						p.active[name] = -age
					}
				}
			}
		}
		clear(results)
		age++
	}
}

func (p *builtinCatalog) removeHistoricalDefinitions(req llm.CompletionRequest) llm.CompletionRequest {
	cloned := false
	for _, r := range discoveryHistory(req) {
		contracts, ok := retainedContracts(r.text)
		if !ok {
			continue
		}
		var keep []discoveredContract
		var native []string
		for _, c := range contracts {
			if p.options.Known[c.Name] {
				native = append(native, c.Name)
			} else {
				keep = append(keep, c)
			}
		}
		if len(native) == 0 {
			continue
		}
		var value any = map[string]any{"native_definitions_omitted": native, "message": "内置工具参数以本轮工具声明为准；未声明时用 tool_search 精确加载。"}
		if len(keep) > 0 {
			value = retainedDiscovery{ContractAvailable: true, Note: historicalToolHelpNote, Tools: keep}
		}
		raw, _ := json.Marshal(value)
		if !cloned {
			req = cloneToolMessages(req)
			cloned = true
		}
		b := &req.Messages[r.message].Content[r.block]
		b.Content = append([]llm.ContentBlock(nil), b.Content...)
		b.Content[0].Text = string(raw)
	}
	return req
}

// Host-owned writeback set; entering settlement must not activate every optional
// probe. The SDK's disabled tools and live authorization remain authoritative.
func builtinSettlementTool(name string) bool {
	return slices.Contains([]string{"record_fact", "insert_assets", "report_finding", "update_finding_report", "set_goals", "set_constraints", "add_hint", "add_task_hint", "prove_goal"}, name)
}
