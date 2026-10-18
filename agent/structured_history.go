package agent

import (
	"encoding/json"

	"github.com/Autumn-27/norma/llm"
)

// SDK compaction can shorten JSON without preserving its authorization IDs.
// Never interpret a parse failure as permission to keep a sensitive preview.
// Only SERIES structured readers/writes are covered; arbitrary Bash/MCP text
// and ordinary error diagnostics keep their existing semantics.
func redactUnverifiableStructuredHistory(req llm.CompletionRequest) llm.CompletionRequest {
	calls := map[string]string{}
	cloned := false
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls[b.ID] = assetContextCall(b).Name
			}
			name := calls[b.ToolUseID]
			if b.Type != llm.BlockToolResult || b.IsError || (!assetStructuredTool(name) && name != "traffic_get" && name != "traffic_search") {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText || c.Text == "" {
					continue
				}
				if json.Valid([]byte(c.Text)) {
					continue
				}
				if !cloned {
					req = cloneToolMessages(req)
					cloned = true
				}
				block := &req.Messages[mi].Content[bi]
				block.Content = append([]llm.ContentBlock(nil), block.Content...)
				block.Content[ci].Text = `{"context_unavailable":true,"message":"该历史结构化结果已损坏或被压缩截断，无法复核资产权限；请按原调用参数重新读取或缩小范围"}`
			}
		}
	}
	return req
}
