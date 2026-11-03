package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/Autumn-27/norma/memory"
)

func (t *ToolSet) workerLaunchContext(ctx context.Context, intentID int64) string {
	target, err := t.contextTarget(ctx, "", "detail")
	if err != nil {
		return "\n关联事实读取失败。"
	}
	parents, err := target.ts.ContextParents(ctx, intentID, 0, 8)
	if err != nil {
		return "\n关联事实读取失败；可通过 context_get 按已知引用重试。"
	}
	more := len(parents) > 8
	if more {
		parents = parents[:8]
	}
	for len(parents) > 0 {
		raw, _ := json.Marshal(parents)
		if utf8.RuneCount(raw) <= 3300 {
			break
		}
		parents = parents[:len(parents)-1]
		more = true
	}
	result := map[string]any{"parents": parents, "has_more": more}
	if more && len(parents) > 0 {
		result["next"] = map[string]any{"items": []contextRead{{Ref: fmt.Sprintf("node:%d", intentID), View: "parents", Before: parents[len(parents)-1].ID}}}
	}
	b, _ := json.Marshal(result)
	return "\n【当前意图的上游事实/漏洞摘要】仅执行当前意图；需要证据时用 context_get，其他上下文按需检索。\n" + string(b)
}

func workerMemoryPreview(store *memory.Store, query string) string {
	if store == nil {
		return ""
	}
	items := store.Relevant(query, 3)
	if len(items) == 0 {
		return ""
	}
	budget := 1600
	var text string
	for _, m := range items {
		if budget <= 0 {
			break
		}
		preview := firstLine(m.Content, min(500, budget))
		line := fmt.Sprintf("\n记忆 %s（非实时证据，完整内容用RecallMemory）：%s", firstLine(m.Name, 80), preview)
		if utf8.RuneCountInString(line) > budget {
			continue
		}
		text += line
		budget -= utf8.RuneCountInString(line)
	}
	return "\n【相关经验预览：不代表当前目标事实】" + text
}
