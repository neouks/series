package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/runtimebudget"
)

// newSession integrates SERIES policy through public SDK extension points only.
// The official dispatcher calls targets directly, so our outer hook must resolve
// their identity before applying business policy. No SDK source is replaced.
func newSession(opts agentcore.Options) (conversationSession, func(), error) {
	configureWorkerMemory(&opts)
	// An empty catalog is authoritative. The SDK interprets nil as a request
	// for its default filesystem/Bash tools, which would bypass role bindings.
	if opts.Tools == nil {
		opts.Tools = []actool.CoreTool{}
	}
	configureDeferredSettlement(&opts)
	cleanup := func() {}
	var ptys *ptyOwner
	if hasOwnedPTYTools(opts.Tools) {
		ptys = newPTYOwner()
	}
	if ptys == nil && toolsNeedTaskManager(opts.Tools) && !actool.InteractiveShellDisabled() &&
		(opts.DisableBackgroundTasks || actool.BackgroundTasksDisabled()) {
		manager, err := actool.NewManager(opts.SessionID)
		if err != nil {
			return nil, cleanup, fmt.Errorf("interactive shell manager: %w", err)
		}
		owner := newInteractiveOwner(manager)
		cleanup = owner.close
		opts.Tools = append([]actool.CoreTool(nil), opts.Tools...)
		for i, tool := range opts.Tools {
			switch tool.Name() {
			case "shell_open", "shell_send", "shell_read", "shell_close", "shell_list":
				opts.Tools[i] = &interactiveTool{CoreTool: tool, owner: owner}
			}
		}
	}
	if len(opts.DeferredTools) > 0 && opts.UnlockSet == nil {
		opts.UnlockSet = actool.NewUnlockSet(opts.DeferredTools...)
	}
	policy := newDispatchHooks(opts)
	policy.ptys = ptys
	// Only supply recheck context here: replacing Call would bypass the existing
	// binding/default/usage decorators. Deferred targets use this registry too.
	opts.Tools = append([]actool.CoreTool{}, opts.Tools...)
	for i, tool := range opts.Tools {
		opts.Tools[i] = &processContextTool{CoreTool: tool, hooks: policy, background: !opts.DisableBackgroundTasks && !actool.BackgroundTasksDisabled()}
		policy.tools[tool.Name()] = opts.Tools[i]
	}
	configureSmallWindowCompaction(&opts)
	configureCompactionUsage(&opts)
	policy.observeModelCalls(&opts)
	opts.Hooks = policy
	if opts.Transcript != nil || ptys != nil || (!opts.DisableBackgroundTasks && !actool.BackgroundTasksDisabled()) {
		session, err := newHostedSession(opts, policy)
		if err != nil {
			if ptys != nil {
				ptys.close()
			}
			cleanup()
			return nil, func() {}, fmt.Errorf("session initialization: %w", err)
		}
		return session, func() { session.Close(); cleanup() }, nil
	}
	session := agentcore.NewSession(opts)
	return session, func() { session.Close(); cleanup() }, nil
}

type processContextTool struct {
	actool.CoreTool
	hooks      *dispatchHooks
	background bool
	ptyOnce    sync.Once
	ptySchema  map[string]any
}

func (t *processContextTool) interactiveSchema() map[string]any {
	// Tools/configuration are resolved per run. Cache only on this immutable
	// instance, never globally across roles, tasks or later settings changes.
	t.ptyOnce.Do(func() { t.ptySchema = interactiveInputSchema(t.Name(), t.CoreTool.InputSchema()) })
	return t.ptySchema
}

func (t *processContextTool) InputSchema() map[string]any {
	if isInteractiveTool(t.Name()) {
		return t.interactiveSchema()
	}
	schema := t.CoreTool.InputSchema()
	if t.Name() != "Bash" || t.background {
		return schema
	}
	// The official schema only knows the global switch, not the per-role
	// session setting. Do not advertise an unavailable background execution path.
	copy := make(map[string]any, len(schema))
	for key, value := range schema {
		copy[key] = value
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		p := make(map[string]any, len(props))
		for key, value := range props {
			if key != "run_in_background" {
				p[key] = value
			}
		}
		copy["properties"] = p
	}
	if raw, err := json.Marshal(schema["required"]); err == nil {
		var required []string
		if json.Unmarshal(raw, &required) == nil {
			filtered := make([]string, 0, len(required))
			for _, name := range required {
				if name != "run_in_background" {
					filtered = append(filtered, name)
				}
			}
			if schema["required"] != nil {
				copy["required"] = filtered
			}
		}
	}
	return copy
}

func (t *processContextTool) PrepareInput(input json.RawMessage) (json.RawMessage, error) {
	resolved, err := PrepareToolInput(t.CoreTool, input)
	if err == nil && isInteractiveTool(t.Name()) {
		err = validateInteractiveBoundedInput(t.Name(), t.interactiveSchema(), resolved)
	}
	return resolved, err
}
func (t *processContextTool) AcceptsRawInput() bool {
	if isInteractiveTool(t.Name()) {
		return true // PrepareInput already validates the full schema and bounds.
	}
	raw, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && raw.AcceptsRawInput()
}
func (t *processContextTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if t.hooks != nil && t.hooks.builtins != nil {
		ctx = context.WithValue(ctx, builtinSessionKey{}, t.hooks.builtins)
	}
	if isInteractiveTool(t.Name()) {
		if err := ctx.Err(); err != nil {
			return actool.Errorf("interactive shell execution canceled: " + err.Error()), nil
		}
		// Keep the guard at the actual registry call too, including deferred calls.
		// Preparation does not run approvals, metering or the underlying executor.
		var err error
		input, err = t.PrepareInput(input)
		if err != nil {
			return actool.Errorf("invalid interactive shell input: " + err.Error()), nil
		}
	}
	check := processRecheck(func(checkCtx context.Context, name string, in []byte) string {
		if t.hooks.builtins != nil {
			if status, err := t.hooks.builtins.check(checkCtx, name); err != nil || status != "" {
				return "工具授权已变化，禁止执行: " + status
			}
		}
		if policy, ok := t.hooks.inner.(interface {
			RecheckToolInput(context.Context, string, []byte) string
		}); ok {
			return policy.RecheckToolInput(checkCtx, name, in)
		}
		if RunInfoFrom(checkCtx).TaskID > 0 {
			return "queued task command has no trusted authorization recheck; not executed"
		}
		return ""
	})
	ctx = context.WithValue(ctx, processRecheckKey{}, check)
	if t.hooks != nil && t.hooks.ptys != nil {
		ctx = context.WithValue(ctx, ptyOwnerKey{}, t.hooks.ptys)
	}
	return t.CoreTool.Call(ctx, input, tc)
}

type interactiveTool struct {
	actool.CoreTool
	owner *interactiveOwner
}

// The host owns this manager (the SDK does not see it). Drain its calls before
// cleanup so a canceled opening cannot publish a PTY after cleanup has passed.
type interactiveOwner struct {
	manager *actool.Manager
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	once    sync.Once
}

func newInteractiveOwner(manager *actool.Manager) *interactiveOwner {
	ctx, cancel := context.WithCancel(context.Background())
	return &interactiveOwner{manager: manager, ctx: ctx, cancel: cancel}
}

func (o *interactiveOwner) close() {
	o.once.Do(func() {
		o.mu.Lock()
		o.closed = true
		o.cancel()
		o.mu.Unlock()
		o.wg.Wait()
		o.manager.Cleanup()
	})
}

func (t *interactiveTool) PrepareInput(input json.RawMessage) (json.RawMessage, error) {
	return PrepareToolInput(t.CoreTool, input)
}

func (t *interactiveTool) AcceptsRawInput() bool {
	raw, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && raw.AcceptsRawInput()
}

func (t *interactiveTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	t.owner.mu.Lock()
	if t.owner.closed {
		t.owner.mu.Unlock()
		return actool.Errorf("interactive session is closed"), nil
	}
	t.owner.wg.Add(1)
	t.owner.mu.Unlock()
	defer t.owner.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.owner.ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := ctx.Err(); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var local actool.ToolContext
	if tc != nil {
		local = *tc
	}
	local.Tasks = t.owner.manager
	return t.CoreTool.Call(ctx, input, &local)
}

type dispatchHooks struct {
	ptys           *ptyOwner
	inner          harness.HookRunner
	tools          map[string]actool.CoreTool
	deferred       map[string]bool
	unlock         *actool.UnlockSet
	permissions    permission.Context
	ask            permission.CanUseTool
	viewMu         sync.RWMutex
	visible        map[string]bool
	phaseTargets   map[string]bool
	schemaRequired map[string]bool
	schemaVisible  map[string]bool
	schemaPressure toolSchemaPressure
	builtins       *builtinCatalog
}

// The official SDK hides Settlement.DisabledTools from request schemas but
// leaves them executable in its registry. Enforce the host's per-request
// surface in the hook as well. Observe only the loop's model calls (not the
// compactor's summarizer); use trusted schemas, never model/user message text.
func (h *dispatchHooks) observeModelCalls(opts *agentcore.Options) {
	if h.builtins != nil && h.builtins.options.Mode != "direct" {
		cached := opts.DynamicBoundary == len(opts.SystemPrompt)
		opts.SystemPrompt = append(append([]string(nil), opts.SystemPrompt...), builtinLoadingRule)
		if cached {
			opts.DynamicBoundary = len(opts.SystemPrompt)
		}
	}
	stream, complete := opts.Deps.CallModel, opts.Deps.CallModelSync
	if opts.Provider != nil {
		if stream == nil {
			stream = opts.Provider.Stream
		}
		if complete == nil {
			complete = opts.Provider.Complete
		}
	}
	if stream != nil {
		opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			if err := intentInputRetryError(ctx); err != nil {
				return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, err) }
			}
			if h.builtins != nil {
				settling := h.phaseTargets != nil
				for _, schema := range req.Tools {
					if schema.Name == settlementMarkerName {
						settling = false
					}
				}
				var err error
				req, err = h.builtins.project(ctx, req, settling)
				if err != nil {
					return func(yield func(llm.StreamEvent, error) bool) { yield(llm.StreamEvent{}, err) }
				}
			}
			req = h.projectToolDocumentation(req)
			if h.builtins != nil {
				req = h.builtins.applyPressure(req)
			}
			h.observeTools(req.Tools)
			req = withoutSettlementMarker(req)
			h.observeDiscoveredSchemas(req)
			return committedModelStream(ctx, req, stream)
		}
	}
	if complete != nil {
		opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			if err := intentInputRetryError(ctx); err != nil {
				return llm.Message{}, "", llm.Usage{}, err
			}
			if h.builtins != nil {
				settling := h.phaseTargets != nil
				for _, schema := range req.Tools {
					if schema.Name == settlementMarkerName {
						settling = false
					}
				}
				var err error
				req, err = h.builtins.project(ctx, req, settling)
				if err != nil {
					return llm.Message{}, "", llm.Usage{}, err
				}
			}
			req = h.projectToolDocumentation(req)
			if h.builtins != nil {
				req = h.builtins.applyPressure(req)
			}
			h.observeTools(req.Tools)
			req = withoutSettlementMarker(req)
			h.observeDiscoveredSchemas(req)
			callCtx, release, err := acquireModelRequest(ctx, runtimebudget.ModelGate(), false)
			if err != nil {
				return llm.Message{}, "", llm.Usage{}, err
			}
			defer release()
			msg, stop, usage, err := complete(callCtx, req)
			return msg, stop, usage, modelRequestError(callCtx, err)
		}
	}
}

func (h *dispatchHooks) observeTools(schemas []llm.ToolSchema) {
	visible := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		visible[schema.Name] = true
	}
	h.viewMu.Lock()
	h.visible = visible
	h.viewMu.Unlock()
}

func newDispatchHooks(opts agentcore.Options) *dispatchHooks {
	h := &dispatchHooks{inner: opts.Hooks, tools: map[string]actool.CoreTool{}, deferred: map[string]bool{}, unlock: opts.UnlockSet,
		permissions: permission.Context{Mode: opts.PermissionMode, WorkingDir: opts.WorkingDir, Allowed: opts.AllowedTools, Disallowed: opts.DisallowedTools}, ask: opts.CanUseTool}
	h.schemaPressure.window = defaultWindowK * 1000
	if opts.Compaction != nil && opts.Compaction.ContextWindow > 0 {
		h.schemaPressure.window = opts.Compaction.ContextWindow
	}
	if c, ok := opts.Compactor.(*contextWindowCompactor); ok && c.window > 0 {
		h.schemaPressure.window = c.window
	}
	for _, t := range opts.Tools {
		h.tools[t.Name()] = t
		if h.builtins == nil {
			if p := builtinPolicy(t); p != nil {
				h.builtins = p.fresh()
				h.builtins.window = h.schemaPressure.window
			}
		}
	}
	for _, name := range opts.DeferredTools {
		h.deferred[name] = true
	}
	// Only the application's OnDemandTools catalog opts into schema eviction.
	// Explicit SDK-only tools keep their existing programmatic call contract.
	h.schemaRequired = map[string]bool{}
	for _, t := range opts.Tools {
		if requiresDiscoveredSchema(t) && h.deferred[t.Name()] {
			h.schemaRequired[t.Name()] = true
		}
	}
	if h.tools[settlementMarkerName] != nil && opts.Settlement != nil {
		h.phaseTargets = map[string]bool{}
		for _, name := range opts.Settlement.DisabledTools {
			h.phaseTargets[name] = true
		}
	}
	return h
}

type deferredInvocation struct {
	Name   string          `json:"tool_name"`
	Params json.RawMessage `json:"params"`
}

func (h *dispatchHooks) target(name string, input []byte) (string, json.RawMessage, error) {
	if name == actool.ExecuteExtraToolName {
		var call deferredInvocation
		if err := json.Unmarshal(input, &call); err != nil {
			return "", nil, err
		}
		name, input = call.Name, call.Params
		if name == actool.ExecuteExtraToolName || name == actool.SearchExtraToolsName || !h.deferred[name] || h.tools[name] == nil {
			return "", nil, fmt.Errorf("tool is not bound as a deferred capability")
		}
		if len(input) == 0 {
			input = []byte(`{}`)
		}
	}
	// Direct calls that guess a deferred name must obey the same skill gate.
	if h.deferred[name] && (h.unlock == nil || !h.unlock.Has(name)) {
		return "", nil, fmt.Errorf("tool %s is not unlocked in this session", name)
	}
	return name, input, nil
}

func (h *dispatchHooks) PreToolUse(ctx context.Context, outer string, input []byte) (bool, string, []byte) {
	if err := ctx.Err(); err != nil {
		return true, "tool execution canceled: " + err.Error(), nil
	}
	name, params, err := h.target(outer, input)
	if err != nil {
		return true, err.Error(), nil
	}
	h.viewMu.RLock()
	hidden := outer == settlementMarkerName || (h.visible != nil && !h.visible[outer] && !h.deferred[outer])
	phaseBlocked := h.visible != nil && h.phaseTargets[name] && !h.visible[settlementMarkerName]
	if phaseBlocked {
		hidden = true
	}
	missingSchema := h.schemaRequired[name] && !h.schemaVisible[name]
	h.viewMu.RUnlock()
	if h.builtins != nil {
		if status, err := h.builtins.check(ctx, name); err != nil || status != "" {
			return true, "内置工具权限或服务状态已变化，未执行: " + status, nil
		}
		if hidden && h.builtins.options.Known[name] && !phaseBlocked {
			return true, "内置工具尚未加载或因上下文压力退出声明；先用 tool_search({\"mode\":\"exact\",\"query\":\"" + name + "\"})，下一轮读取完整参数后调用。", nil
		}
	}
	if hidden {
		return true, "tool is not available in the current execution phase", nil
	}
	if missingSchema {
		query, _ := json.Marshal(map[string]string{"query": "select:" + name})
		return true, "tool schema is absent or outdated in this model request; call SearchExtraTools(" + string(query) + "), read its complete schema, then invoke in the next model response. Do not guess parameters.", nil
	}
	tool := h.tools[name]
	validate := func(in json.RawMessage) (json.RawMessage, error) {
		if tool == nil {
			return in, nil
		} // SDK-injected tools keep official validation.
		resolved, err := PrepareToolInput(tool, in)
		if err != nil {
			return nil, err
		}
		if raw, ok := tool.(interface{ AcceptsRawInput() bool }); !ok || !raw.AcceptsRawInput() {
			err = actool.ValidateInput(tool.InputSchema(), resolved)
		}
		return resolved, err
	}
	params, err = validate(params)
	if err != nil {
		if name == "add_intent" {
			return true, intentInputError(err.Error()), nil
		}
		return true, "invalid tool input: " + err.Error(), nil
	}
	if outer == actool.ExecuteExtraToolName {
		decision := permission.Evaluate(ctx, permission.Request{ToolName: name, Input: params, IsReadOnly: tool.IsReadOnly(params),
			ToolDecision: tool.CheckPermissions(ctx, params, h.permissions), Ctx: h.permissions, Ask: h.ask})
		if decision.Behavior != permission.Allow {
			return true, decision.Message, nil
		}
		if len(decision.UpdatedInput) > 0 {
			params, err = validate(decision.UpdatedInput)
			if err != nil {
				return true, "invalid permission input: " + err.Error(), nil
			}
		}
	}
	if h.inner != nil {
		blocked, reason, updated := h.inner.PreToolUse(ctx, name, params)
		if blocked {
			return true, reason, nil
		}
		if len(updated) > 0 {
			params, err = validate(updated)
			if err != nil {
				return true, "invalid updated tool input: " + err.Error(), nil
			}
		}
		// Approval can wait for human input. Recheck even unchanged arguments:
		// the user may have revoked the target while the action was waiting.
		// This is authorization only, not a second action approval or tool count.
		if policy, ok := h.inner.(interface {
			RecheckToolInput(context.Context, string, []byte) string
		}); ok {
			if reason := policy.RecheckToolInput(ctx, name, params); reason != "" {
				return true, reason, nil
			}
		}
	}
	if h.builtins != nil {
		if status, err := h.builtins.check(ctx, name); err != nil || status != "" {
			return true, "工具授权已变化，未执行: " + status, nil
		}
	}
	if outer == actool.ExecuteExtraToolName {
		envelope, _ := json.Marshal(deferredInvocation{Name: name, Params: params})
		return false, "", envelope
	}
	return false, "", params
}

func (h *dispatchHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.builtins != nil && !isErr {
		if target, _, err := h.target(name, input); err == nil {
			h.builtins.used(target)
		}
	}
	if h.inner == nil {
		return
	}
	target, params, err := h.target(name, input)
	if err == nil {
		h.inner.PostToolUse(ctx, target, params, result, isErr)
	}
}

func (h *dispatchHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner != nil {
		return h.inner.Stop(ctx, messages)
	}
	return false, nil, ""
}
