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

func workerMemoryPreview(ctx context.Context, store *memory.Store, query string) string {
	if store == nil {
		return ""
	}
	previewCtx, cancel := memoryPreviewContext(ctx)
	defer cancel()
	selection, err := selectMemoryPreviews(previewCtx, store, query)
	if err != nil {
		return "\n【经验预览暂不可用】未加载经验不代表没有任务事实；已保存记忆仍可用RecallMemory按需读取。"
	}
	if len(selection.items) == 0 && !selection.more {
		return ""
	}
	budget := 1600
	text := fmt.Sprintf("\n【相关经验预览：非当前目标事实】按名称/描述字面匹配；索引%d条，待更新%d条，不可读%d条，仍有未展示=%t。正文未参与自动搜索，未匹配不等于没有经验。", selection.indexed, selection.pending, selection.unreadable, selection.more)
	budget -= utf8.RuneCountInString(text)
	for i, m := range selection.items {
		if budget <= 0 {
			break
		}
		preview := firstLine(m.Content, min(500, budget))
		line := fmt.Sprintf("\n记忆 %s；原文入口 %s：%s", firstLine(m.Name, 80), memoryPreviewRead(selection.files[i], selection.versions[i]), preview)
		if utf8.RuneCountInString(line) > budget {
			continue
		}
		text += line
		budget -= utf8.RuneCountInString(line)
	}
	return text
}
