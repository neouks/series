package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

const manualVerificationNotice = "此操作需要人工确认和处理。请说明待核对的问题与缺少的资料，由用户处理后提供结果。"
const humanConfirmationGuidance = "\n需要人工协助时，先写回已有结论，再用 request_human_confirmation 记录具体问题和缺少的资料，随后结束本轮。收到用户补充的材料后，可继续分析。"

type humanConfirmationKey struct{}

type humanConfirmation struct {
	mu     sync.Mutex
	reason string
}

func (h *humanConfirmation) record(reason string, explicit bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if explicit || h.reason == "" {
		h.reason = reason
	}
}

func (h *humanConfirmation) pending() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reason
}

func (h *humanConfirmation) annotateResult(a db.Activity) db.Activity {
	if a.Kind != "result" {
		return a
	}
	if reason := h.pending(); reason != "" {
		a.Summary = "待人工确认"
		a.Detail = "待人工确认\n\n" + reason + "\n\n" + a.Detail
	}
	return a
}

func (h *humanConfirmation) tool() actool.CoreTool {
	return writeTool("request_human_confirmation", "记录需要用户确认的问题及缺少的资料。随后保存已有结论并结束本轮。", obj(map[string]any{
		"reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
	}, "reason"), func(_ context.Context, raw json.RawMessage) (actool.Result, error) {
		var input struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return actool.Errorf("参数错误：需要提供 reason"), nil
		}
		reason := strings.TrimSpace(input.Reason)
		if reason == "" || utf8.RuneCountInString(reason) > 1000 {
			return actool.Errorf("reason 需为 1–1000 字符"), nil
		}
		h.record(reason, true)
		return actool.Text("已登记待人工确认事项。保存已有结论后结束本轮。"), nil
	})
}

// Only capabilities that work on existing material or record local state are
// eligible. A tool's read-only flag does not establish that it is offline.
func checkHumanConfirmation(ctx context.Context, name string) string {
	if !RequiresManualVerification(RunInfoFrom(ctx).AgentKey) {
		return ""
	}
	if passiveTaskTool(name) {
		return ""
	}
	return requireManualVerification(ctx)
}

func passiveTaskTool(name string) bool {
	switch name {
	case "context_search", "context_get", "asset_search", "asset_get", "list_companies",
		"list_task_assets", "check_target_access", "list_finding_deletion_feedback",
		"graph_overview", "traffic_search", "traffic_get", "get_finding_traffic",
		"insert_assets", "record_fact", "report_finding", "add_task_scope", "add_company_scope",
		"add_intent", "prove_goal", "kill_work", "steer_work",
		"Read", "LS", "Glob", "Grep", "TodoWrite", "RecallMemory", "RecordMemory",
		"Skill", "Compress", actool.SearchExtraToolsName, "request_human_confirmation":
		return true
	}
	return false
}

func RequiresManualVerification(role string) bool {
	return role == "planner" || role == "worker"
}

// RequireManualVerification also guards host executors whose custom names may
// overlap with built-in tools.
func RequireManualVerification(ctx context.Context) string {
	if !RequiresManualVerification(RunInfoFrom(ctx).AgentKey) {
		return ""
	}
	return requireManualVerification(ctx)
}

func requireManualVerification(ctx context.Context) string {
	if h, ok := ctx.Value(humanConfirmationKey{}).(*humanConfirmation); ok {
		h.record(manualVerificationNotice, false)
	}
	return manualVerificationNotice
}
