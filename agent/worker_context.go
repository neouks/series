package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

func (t *ToolSet) workerLaunchContext(ctx context.Context, intentID int64) string {
	target, err := t.contextTarget(ctx, "", "detail")
	if err != nil {
		return "\n关联事实读取失败。"
	}
	parents, err := target.ts.ContextParents(ctx, intentID, 0, 20)
	if err != nil {
		return "\n关联事实读取失败；可通过 context_get 按已知引用重试。"
	}
	more := len(parents) > 20
	if more {
		parents = parents[:20]
	}
	result := map[string]any{"parents": parents, "has_more": more}
	if more {
		result["next"] = map[string]any{"items": []contextRead{{Ref: fmt.Sprintf("node:%d", intentID), View: "parents", Before: parents[len(parents)-1].ID}}}
	}
	b, _ := json.Marshal(result)
	return "\n【当前意图的上游事实/漏洞摘要】仅执行当前意图；需要证据时用 context_get，其他上下文按需检索。\n" + string(b)
}
