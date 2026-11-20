//go:build liveeval

package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"github.com/neouks/series/db"
	"github.com/neouks/series/mcphttp"
)

type liveEvaluationConfig struct {
	ContextWindowK   int    `json:"context_window_k"`
	Format           string `json:"format"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	APIKey           string `json:"api_key"`
	APIKeyEnv        string `json:"api_key_env"`
	APIKeyFile       string `json:"api_key_file"`
	Proxy            string `json:"proxy"`
	ReasoningEffort  string `json:"reasoning_effort"`
	ThinkingType     string `json:"thinking_type"`
	MaxTokensField   string `json:"max_tokens_field"`
	MaxTokens        int    `json:"max_tokens"`
	SessionHeaderKey string `json:"session_header_key"`
}

type liveFixture struct {
	mu              sync.Mutex
	steps           []int
	position        int
	seed            string
	record          string
	finalReceipt    string
	backendCalls    int
	backendRejected int
	successByKind   map[string]int
	tools           []actool.CoreTool
	kinds           map[string]string
	server          *httptest.Server
	client          *mcphttp.Client
}

func liveReceipt(seed string, position int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, position)))
	return hex.EncodeToString(h[:8])
}

func liveSchema(index int) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"record_id": map[string]any{"type": "string", "description": "原样使用任务提供的记录编号"},
		"ticket":    map[string]any{"type": "string", "description": "上一步返回的动态票据，禁止自行编造"},
		"mode":      map[string]any{"type": "string", "enum": []string{fmt.Sprintf("mode_%02d", index)}},
		"limit":     map[string]any{"type": "integer", "default": 3, "minimum": 3, "maximum": 3},
		"options":   map[string]any{"type": "object", "properties": map[string]any{"detail": map[string]any{"type": "string", "enum": []string{"compact", "full"}, "description": "取值规则见完整工具说明"}}, "required": []string{"detail"}, "additionalProperties": false},
	}, "required": []string{"record_id", "ticket", "mode", "limit", "options"}, "additionalProperties": false}
}

func liveDescription(index int) string {
	return fmt.Sprintf("能力 cap_%02d：读取并推进记录处理链中的指定阶段。只在下一步指向这个能力时调用。\n", index) +
		strings.Repeat("参考说明：每次操作读取当前票据并返回新票据，不能缓存执行结果替代实际调用。记录编号与票据需要逐字保留。操作返回的 next 是下一阶段入口，不是本工具的额外参数。不要一次生成尚未取得票据的后续调用。\n", 12) +
		"业务规则：record_id 以 R 开头时 options.detail 必须是 full；其他记录使用 compact。limit 固定为 3。成功时继续处理 next，done 为 true 时停止并返回 final_receipt。"
}

func (f *liveFixture) execute(index int, input map[string]any) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backendCalls++
	options, _ := input["options"].(map[string]any)
	valid := f.position < len(f.steps) && f.steps[f.position] == index && input["record_id"] == f.record &&
		input["ticket"] == liveReceipt(f.seed, f.position) && input["mode"] == fmt.Sprintf("mode_%02d", index) && options["detail"] == "full" && input["limit"] == float64(3)
	if !valid {
		f.backendRejected++
		return map[string]any{"error": "参数或调用顺序不符合当前记录契约；核对完整说明、Schema 和上一步的 next。"}, false
	}
	kind := "custom_http"
	if index == 0 {
		kind = "custom_command"
	} else if index%2 == 1 {
		kind = "external_mcp"
	}
	f.successByKind[kind]++
	f.position++
	if f.position == len(f.steps) {
		return map[string]any{"done": true, "final_receipt": f.finalReceipt}, true
	}
	return map[string]any{"done": false, "next": map[string]any{"capability": fmt.Sprintf("cap_%02d", f.steps[f.position]), "record_id": f.record, "ticket": liveReceipt(f.seed, f.position)}}, true
}

func newLiveFixture(t *testing.T, scenario string, repeat int) *liveFixture {
	t.Helper()
	f := &liveFixture{seed: fmt.Sprintf("live-tools-v1/%s/%s/%d", os.Getenv("ARTEX_LIVE_CAMPAIGN"), scenario, repeat), record: "R-1042", successByKind: map[string]int{}, kinds: map[string]string{}}
	switch scenario {
	case "short_mixed":
		f.steps = []int{0, 1, 2}
	case "repeated_tools":
		f.steps = []int{0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3}
	case "catalog_churn":
		f.steps = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0, 1, 2}
	default:
		t.Fatalf("unknown scenario %s", scenario)
	}
	f.finalReceipt = liveReceipt(f.seed, 999)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/custom/") {
			index, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/custom/"))
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			output, ok := f.execute(index, input)
			if !ok {
				w.WriteHeader(422)
			}
			_ = json.NewEncoder(w).Encode(output)
			return
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&rpc) != nil {
			http.Error(w, "invalid RPC", 400)
			return
		}
		if rpc.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "isolated-evaluation", "version": "1"}}
		case "tools/list":
			var list []any
			for i := 1; i < 16; i += 2 {
				list = append(list, map[string]any{"name": fmt.Sprintf("cap_%02d", i), "description": liveDescription(i), "inputSchema": liveSchema(i)})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(rpc.Params, &call)
			index, _ := strconv.Atoi(strings.TrimPrefix(call.Name, "cap_"))
			output, ok := f.execute(index, call.Arguments)
			body, _ := json.Marshal(output)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}, "isError": !ok}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(f.server.Close)
	// The command tool really launches the platform's command executor; this
	// small local script sends its rendered arguments to the isolated fixture.
	script := filepath.Join(t.TempDir(), "fixture.py")
	code := "import json,sys,urllib.request,urllib.error\nu=sys.argv[1]\np=dict(record_id=sys.argv[2],ticket=sys.argv[3],mode=sys.argv[4],options=json.loads(sys.argv[5]),limit=int(sys.argv[6]))\nr=urllib.request.Request(u,data=json.dumps(p).encode(),headers={'Content-Type':'application/json'})\no=urllib.request.build_opener(urllib.request.ProxyHandler({}))\ntry:\n print(o.open(r,timeout=10).read().decode())\nexcept urllib.error.HTTPError as e:\n print(e.read().decode());sys.exit(1)\n"
	if err := os.WriteFile(script, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	for i := 0; i < 16; i += 2 {
		kind := "http"
		var spec any = httpExec{Method: "POST", URL: fmt.Sprintf("%s/custom/%d", f.server.URL, i), Headers: map[string]string{"Content-Type": "application/json"}, Body: `{"record_id":"{record_id}","ticket":"{ticket}","mode":"{mode}","options":{options},"limit":{limit}}`}
		if i == 0 {
			kind = "command"
			spec = commandExec{Command: "python3 " + shellQuote(script) + " " + shellQuote(f.server.URL+"/custom/0") + " {record_id} {ticket} {mode} {options} {limit}", TimeoutMs: 10000}
		}
		raw, _ := json.Marshal(spec)
		schema, _ := json.Marshal(liveSchema(i))
		row := &db.Tool{Key: fmt.Sprintf("custom_cap_%02d", i), Kind: kind, Description: liveDescription(i), Schema: schema, Exec: raw}
		tool := decorateCustomTool(s.buildCustomTool(row), row, liveSchema(i))
		f.tools = append(f.tools, tool)
		f.kinds[tool.Name()] = "custom_" + kind
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client, err := mcphttp.New(ctx, "fixture", f.server.URL+"/mcp", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	f.client = client
	t.Cleanup(func() { _ = client.Close() })
	remote, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range remote {
		f.tools = append(f.tools, tool)
		f.kinds[tool.Name()] = "external_mcp"
	}
	return f
}

type liveMetrics struct {
	Variant                string         `json:"variant"`
	Scenario               string         `json:"scenario"`
	Repeat                 int            `json:"repeat"`
	Passed                 bool           `json:"passed"`
	CompletedSteps         int            `json:"completed_steps"`
	RequiredSteps          int            `json:"required_steps"`
	Requests               int            `json:"model_requests"`
	ToolCalls              int            `json:"tool_calls"`
	Discoveries            int            `json:"discovery_calls"`
	RepeatedDiscoveries    int            `json:"repeated_discoveries"`
	DiscoverySuccesses     int            `json:"discovery_successes"`
	ExecutionAttempts      int            `json:"execution_attempts"`
	SchemaGateRejections   int            `json:"schema_gate_rejections"`
	InvalidInputRejections int            `json:"invalid_input_rejections"`
	ToolErrors             int            `json:"tool_errors"`
	AttemptsByKind         map[string]int `json:"attempts_by_kind"`
	SuccessByKind          map[string]int `json:"success_by_kind"`
	BackendCalls           int            `json:"backend_calls"`
	BackendRejected        int            `json:"backend_rejected"`
	InputTokens            int            `json:"input_tokens"`
	OutputTokens           int            `json:"output_tokens"`
	CacheReadTokens        int            `json:"cache_read_tokens"`
	CacheWriteTokens       int            `json:"cache_write_tokens"`
	Milliseconds           int64          `json:"milliseconds"`
	RequestBytes           int            `json:"request_bytes"`
	Reason                 string         `json:"reason"`
	Error                  string         `json:"error,omitempty"`
}

func measureLiveRun(run agent.LiveToolRun, f *liveFixture, scenario string, repeat int, elapsed time.Duration, format string) liveMetrics {
	m := liveMetrics{Variant: run.Variant, Scenario: scenario, Repeat: repeat, CompletedSteps: f.position, RequiredSteps: len(f.steps), Requests: len(run.Requests), AttemptsByKind: map[string]int{}, SuccessByKind: f.successByKind, BackendCalls: f.backendCalls, BackendRejected: f.backendRejected,
		InputTokens: run.Usage.InputTokens, OutputTokens: run.Usage.OutputTokens, CacheReadTokens: run.Usage.CacheReadTokens, CacheWriteTokens: run.Usage.CacheWriteTokens, Milliseconds: elapsed.Milliseconds(), Reason: string(run.Reason), Error: run.Error}
	if format == "anthropic" {
		m.InputTokens += run.Usage.CacheReadTokens + run.Usage.CacheWriteTokens
	}
	m.Passed = f.position == len(f.steps) && strings.Contains(run.Final, f.finalReceipt)
	calls := map[string]string{}
	seenTools := map[string]bool{}
	for _, message := range run.Messages {
		for _, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				m.ToolCalls++
				name := block.Name
				if name == actool.ExecuteExtraToolName {
					var in struct {
						Name string `json:"tool_name"`
					}
					_ = json.Unmarshal(block.Input, &in)
					name = in.Name
				}
				calls[block.ID] = name
				if name == actool.SearchExtraToolsName {
					m.Discoveries++
				} else {
					m.ExecutionAttempts++
					m.AttemptsByKind[f.kinds[name]]++
				}
			}
			if block.Type == llm.BlockToolResult {
				text := ""
				for _, part := range block.Content {
					text += part.Text
				}
				if calls[block.ToolUseID] == actool.SearchExtraToolsName && !block.IsError && strings.HasPrefix(text, "Found ") {
					m.DiscoverySuccesses++
					repeated := false
					for _, part := range strings.Split(text, "\n\n## ")[1:] {
						name, _, _ := strings.Cut(part, "\n")
						if seenTools[name] {
							repeated = true
						}
						seenTools[name] = true
					}
					if repeated {
						m.RepeatedDiscoveries++
					}
				}
				if block.IsError {
					m.ToolErrors++
				}
				if strings.Contains(text, "tool schema is absent or outdated") {
					m.SchemaGateRejections++
				}
				if strings.Contains(text, "invalid tool input") || strings.Contains(text, "invalid params") {
					m.InvalidInputRejections++
				}
			}
		}
	}
	for _, request := range run.Requests {
		m.RequestBytes += request.RequestBytes
	}
	return m
}

func TestLiveToolFixtureProtocol(t *testing.T) {
	f := newLiveFixture(t, "short_mixed", 0)
	for position, index := range f.steps {
		name := fmt.Sprintf("custom_cap_%02d", index)
		if index%2 == 1 {
			name = mcphttp.ToolName("fixture", fmt.Sprintf("cap_%02d", index))
		}
		tool, _ := actool.NewRegistry(f.tools...).Get(name)
		raw, _ := json.Marshal(map[string]any{"record_id": f.record, "ticket": liveReceipt(f.seed, position), "mode": fmt.Sprintf("mode_%02d", index), "limit": 3, "options": map[string]string{"detail": "full"}})
		result, err := tool.Call(t.Context(), raw, &actool.ToolContext{WorkingDir: t.TempDir()})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %s", name, err, result.Flatten())
		}
	}
	if f.position != len(f.steps) || f.backendRejected != 0 {
		t.Fatalf("fixture did not complete: %d", f.position)
	}
}

func loadLiveSettings(t *testing.T) liveEvaluationConfig {
	t.Helper()
	var settings liveEvaluationConfig
	if instance := os.Getenv("ARTEX_LIVE_INSTANCE_CONFIG"); instance != "" {
		// TestMain used an isolated/unreachable DSN. Load only the authorized
		// model profile here, in a read-only transaction; never run migrations.
		t.Setenv("SERIES_CONFIG", instance)
		t.Setenv("SERIES_PG_DSN", "")
		dsn, _, err := db.DSN()
		if err != nil {
			t.Fatal("cannot resolve instance configuration")
		}
		conn, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal("cannot open instance configuration database")
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal("cannot read instance configuration database")
		}
		defer tx.Rollback()
		err = tx.QueryRowContext(ctx, `SELECT format,model,COALESCE(base_url,''),COALESCE(api_key,''),COALESCE(proxy,''),COALESCE(reasoning_effort,''),COALESCE(thinking_type,''),COALESCE(max_tokens_field,''),max_tokens,COALESCE(session_header_key,''),context_window_k FROM llm_profiles WHERE is_default LIMIT 1`).Scan(&settings.Format, &settings.Model, &settings.BaseURL, &settings.APIKey, &settings.Proxy, &settings.ReasoningEffort, &settings.ThinkingType, &settings.MaxTokensField, &settings.MaxTokens, &settings.SessionHeaderKey, &settings.ContextWindowK)
		if err != nil {
			t.Fatal("cannot read active model profile")
		}
		return settings
	}
	path := os.Getenv("ARTEX_LIVE_CONFIG")
	if path == "" {
		t.Skip("requires explicit production model configuration")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read live model configuration")
	}
	if json.Unmarshal(raw, &settings) != nil {
		t.Fatal("invalid live model configuration")
	}
	if settings.APIKeyEnv != "" {
		settings.APIKey = os.Getenv(settings.APIKeyEnv)
	}
	if settings.APIKeyFile != "" {
		key, err := os.ReadFile(settings.APIKeyFile)
		if err != nil {
			t.Fatal("cannot read configured key file")
		}
		settings.APIKey = strings.TrimSpace(string(key))
	}
	return settings
}

func TestLiveToolComparison(t *testing.T) {
	settings := loadLiveSettings(t)
	if settings.Model == "" || settings.BaseURL == "" || settings.APIKey == "" {
		t.Fatal("model, base_url and API key source are required")
	}
	cfg := agent.ConfigFrom(settings.Format, settings.Model, settings.BaseURL, settings.APIKey, settings.Proxy)
	cfg.ReasoningEffort, cfg.ThinkingType, cfg.MaxTokensField = settings.ReasoningEffort, settings.ThinkingType, settings.MaxTokensField
	cfg.SessionHeaderKey = settings.SessionHeaderKey
	cfg.ContextWindowK = settings.ContextWindowK
	cfg.Retry = agent.RetryConfig{ConnectAttempts: -1, EmptyAttempts: -1, StreamAttempts: -1}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("ARTEX_LIVE_OUT")
	if out == "" {
		t.Fatal("ARTEX_LIVE_OUT is required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "summary.json")); err == nil {
		t.Fatal("output directory already contains a run; choose a fresh directory")
	}
	repeats := 2
	if raw := os.Getenv("ARTEX_LIVE_REPEATS"); raw != "" {
		repeats, err = strconv.Atoi(raw)
		if err != nil || repeats < 1 || repeats > 5 {
			t.Fatal("repeats must be 1..5")
		}
	}
	scenarios := []string{"short_mixed", "repeated_tools", "catalog_churn"}
	if one := os.Getenv("ARTEX_LIVE_SCENARIO"); one != "" {
		scenarios = []string{one}
	}
	variants := []string{"doc_window", "schema_window", "pressure_window"}
	if raw := os.Getenv("ARTEX_LIVE_VARIANTS"); raw != "" {
		variants = strings.Split(raw, ",")
	}
	maxTokens := settings.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	var metrics []liveMetrics
	for repeat := 0; repeat < repeats; repeat++ {
		for si, scenario := range scenarios {
			for offset := 0; offset < len(variants); offset++ {
				variant := variants[(offset+repeat+si)%len(variants)]
				name := fmt.Sprintf("%s-%s-r%d", scenario, variant, repeat+1)
				t.Run(name, func(t *testing.T) {
					f := newLiveFixture(t, scenario, repeat)
					tools, def := agent.OnDemandTools(f.tools, agent.DeferredInfo{}, "worker")
					system := []string{"你是独立工具调用评测助手。只使用所提供工具完成用户的记录处理任务。延迟工具先用 SearchExtraTools 按名称或关键词获取说明和参数，再用 ExecuteExtraTool 执行。已有完整参数和用法足够时可直接复用，不重复发现。工具返回的 next 提供后续能力和动态票据，不能猜测尚未返回的数据。遇到参数错误应阅读错误并修正。所有任务均为隔离的合成记录。最终答复只返回 final_receipt。", actool.RenderDeferredToolsBlock(def.GlobalNames)}
					prompt := fmt.Sprintf("处理记录 %s。起始能力 cap_00，起始 ticket=%s。按每次工具返回的 next 逐步处理，直到 done=true；不能跳过步骤。最终只返回 final_receipt。", f.record, liveReceipt(f.seed, 0))
					opts := agentcore.Options{Compaction: &compaction.Config{ContextWindow: cfg.CompactionWindow()}, Provider: provider, Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, SystemPrompt: system, DynamicBoundary: len(system), WorkingDir: t.TempDir(), PermissionMode: permission.ModeBypass, MaxTurns: 40, MaxTokens: maxTokens, SessionID: "live-eval-" + name}
					ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
					defer cancel()
					start := time.Now()
					run, err := agent.RunLiveToolEvaluation(ctx, opts, prompt, variant, func(request agent.LiveToolRequest) {
						t.Logf("request %s #%d input=%d output=%d cache=%d ms=%d error=%v", name, request.Turn, request.Usage.InputTokens, request.Usage.OutputTokens, request.Usage.CacheReadTokens, request.Milliseconds, request.Error != "")
						entry, _ := json.Marshal(request)
						file, e := os.OpenFile(filepath.Join(out, name+".requests.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
						if e == nil {
							_, _ = file.Write(append(entry, '\n'))
							_ = file.Close()
						}
					})
					if err != nil {
						t.Fatal(err)
					}
					row := measureLiveRun(run, f, scenario, repeat+1, time.Since(start), settings.Format)
					metrics = append(metrics, row)
					encoded, _ := json.MarshalIndent(run, "", "  ")
					if err := os.WriteFile(filepath.Join(out, name+".json"), encoded, 0600); err != nil {
						t.Fatal(err)
					}
					summary, _ := json.MarshalIndent(map[string]any{"model": settings.Model, "format": settings.Format, "reasoning_effort": settings.ReasoningEffort, "thinking_type": settings.ThinkingType, "transport": "non-streaming", "max_output_tokens": maxTokens, "max_turns": 40, "context_window": cfg.CompactionWindow(), "campaign": os.Getenv("ARTEX_LIVE_CAMPAIGN"), "repeats": repeats, "metrics": metrics}, "", "  ")
					if err := os.WriteFile(filepath.Join(out, "summary.json"), summary, 0600); err != nil {
						t.Fatal(err)
					}
					t.Logf("RESULT %s passed=%v steps=%d/%d requests=%d discoveries=%d executions=%d input=%d output=%d", name, row.Passed, row.CompletedSteps, row.RequiredSteps, row.Requests, row.Discoveries, row.ExecutionAttempts, row.InputTokens, row.OutputTokens)
					if row.Error != "" && row.InputTokens == 0 {
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
