package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	"github.com/neouks/series/db"
)

type contextObserverKey struct{}
type contextOutputDirectoryKey struct{}

func withContextObserver(ctx context.Context, emit func(db.Activity)) context.Context {
	return context.WithValue(ctx, contextObserverKey{}, emit)
}

var budgetWarnings = struct {
	sync.Mutex
	items map[string]time.Time
}{items: map[string]time.Time{}}

type requestFootprint struct{ Characters, Bytes, SystemBytes, ToolsBytes, HistoryBytes int }

func measureRequest(req llm.CompletionRequest) requestFootprint {
	// Marshal one message at a time: measuring a long run must not allocate two
	// additional full-history copies before the projection can reduce it.
	base := req
	base.System = nil
	base.Tools = nil
	base.Messages = nil
	raw, _ := json.Marshal(base)
	sc, sb := measureJSONSlice(req.System)
	tc, tb := measureJSONSlice(req.Tools)
	hc, hb := measureJSONSlice(req.Messages)
	return requestFootprint{Characters: utf8.RuneCount(raw) - 12 + sc + tc + hc, Bytes: len(raw) - 12 + sb + tb + hb, SystemBytes: sb, ToolsBytes: tb, HistoryBytes: hb}
}
func measureJSONSlice[T any](items []T) (int, int) {
	if items == nil {
		return 4, 4
	}
	chars, bytes := 2, 2
	for i, item := range items {
		raw, _ := json.Marshal(item)
		chars += utf8.RuneCount(raw)
		bytes += len(raw)
		if i > 0 {
			chars++
			bytes++
		}
	}
	return chars, bytes
}

// Only complete consecutive tool blocks are collapsed. The latest two batches,
// latest unique diagnostics, user messages and assistant answers remain intact.
// This is a projection; the SDK working set and persisted audit do not change.
func projectCompletedBatches(req llm.CompletionRequest) llm.CompletionRequest {
	return projectCompletedBatchesAt(req, "")
}
func projectCompletedBatchesAt(req llm.CompletionRequest, outputDir string) llm.CompletionRequest {
	type batch struct {
		start, end int
		calls      []llm.ContentBlock
		results    []llm.ContentBlock
		failed     bool
	}
	var batches []batch
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		b := batch{start: i, end: i + 1}
		ids := map[string]bool{}
		valid := true
		for _, c := range m.Content {
			if c.Type == llm.BlockToolUse {
				if c.ID == "" || ids[c.ID] {
					valid = false
				}
				ids[c.ID] = true
				b.calls = append(b.calls, c)
			}
		}
		if len(ids) == 0 || !valid {
			continue
		}
		seen := map[string]bool{}
		for j := i + 1; j < len(req.Messages); j++ {
			next := req.Messages[j]
			if next.Role != llm.RoleUser && next.Role != "tool" {
				break
			}
			if len(next.Content) == 0 {
				break
			}
			only := true
			for _, r := range next.Content {
				if r.Type != llm.BlockToolResult {
					only = false
					break
				}
			}
			if !only {
				break
			}
			for _, r := range next.Content {
				if !ids[r.ToolUseID] || seen[r.ToolUseID] {
					valid = false
				}
				seen[r.ToolUseID] = true
				b.results = append(b.results, r)
				b.failed = b.failed || r.IsError
			}
			b.end = j + 1
		}
		if valid && len(seen) == len(ids) {
			batches = append(batches, b)
			i = b.end - 1
		}
	}
	if len(batches) <= 2 {
		return req
	}
	type diagnostic struct {
		index  int
		failed bool
	}
	latest := map[string]diagnostic{}
	latestError := -1
	for i, b := range batches {
		names := map[string]llm.ContentBlock{}
		for _, c := range b.calls {
			names[c.ID] = assetContextCall(c)
		}
		for _, r := range b.results {
			call := names[r.ToolUseID]
			failed := historicalResultFailed(call, r)
			latest[call.Name] = diagnostic{i, failed}
			if failed {
				latestError = i
			}
		}
	}
	protected := map[int]bool{}
	if latestError >= 0 {
		protected[latestError] = true
	}
	for _, d := range latest {
		if d.failed {
			protected[d.index] = true
		}
	}
	replace := map[int]batch{}
	for i, b := range batches {
		if i >= len(batches)-2 || protected[i] {
			continue
		}
		replace[b.start] = b
	}
	out := make([]llm.Message, 0, len(req.Messages))
	receiptSegments := map[int][]map[string]any{}
	for i := 0; i < len(req.Messages); i++ {
		b, ok := replace[i]
		if !ok {
			out = append(out, req.Messages[i])
			continue
		}
		receipts := []map[string]any{}
		names := map[string]string{}
		calls := map[string]llm.ContentBlock{}
		recoverable := true
		for _, c := range b.calls {
			call := assetContextCall(c)
			names[c.ID] = call.Name
			calls[c.ID] = call
		}
		for _, r := range b.results {
			if !r.IsError && historicalResultFailed(calls[r.ToolUseID], r) {
				// Mixed successful/error items cannot be reduced to a successful
				// receipt without losing item correspondence. Preserve the batch.
				recoverable = false
			}
			entry := map[string]any{"tool": names[r.ToolUseID], "original_call_id": r.ToolUseID, "failed": r.IsError, "history_omitted": true, "original_audit_retained": true}
			refs := []any{}
			call := calls[r.ToolUseID]
			domainResult := assetStructuredTool(call.Name) || durableWriteTool(call.Name)
			if retrievalTool(call.Name) || call.Name == "Read" {
				if utf8.RuneCount(call.Input) <= 1000 && json.Valid(call.Input) {
					entry["reread"] = map[string]any{"tool": call.Name, "args": json.RawMessage(call.Input)}
				}
			}
			var noaRefs []string
			errorHash := sha256.New()
			for _, c := range r.Content {
				if c.Type != llm.BlockText {
					recoverable = false // Image/audio blocks have no equivalent text receipt.
					continue
				}
				plain := noaadapter.StripRefTag(c.Text)
				if r.IsError {
					errorHash.Write([]byte(plain))
				}
				if plain != c.Text {
					noaRefs = append(noaRefs, c.Text[len(plain):])
				}
				var value any
				if decodeStructuredJSON([]byte(plain), &value) == nil {
					if domainResult {
						collectReceiptRefs(value, &refs)
					}
					if m, ok := value.(map[string]any); ok {
						if call.Name == "TodoWrite" {
							if path, ok := m["worker_todo_file"].(string); ok && len(path) < 4096 {
								if info, e := os.Lstat(path); e == nil && info.Mode().IsRegular() {
									addReceiptFile(entry, path)
								}
							}
						}

						if call.Name == "TodoWrite" {
							if ref, ok := m["planner_state_ref"].(string); ok && strings.HasPrefix(ref, "planner_state:") {
								entry["reread"] = map[string]any{"tool": "context_get", "args": map[string]any{"items": []any{map[string]any{"ref": ref, "field": "/todos"}}}}
							}
						}

						for _, key := range []string{"ids", "errors", "duplicates", "status"} {
							if !domainResult {
								break
							}
							if v, ok := m[key]; ok {
								raw, _ := json.Marshal(v)
								if len(raw) <= 1200 {
									entry[key] = v
								} else {
									recoverable = false // Keep all per-item results rather than drop long errors/IDs.
								}
							}
						}
					}
				}
				if match := recordedReference.FindStringSubmatch(plain); len(match) > 1 && (call.Name == "record_fact" || call.Name == "report_finding" || call.Name == "add_intent" || call.Name == "set_goals" || call.Name == "add_hint") {
					if id, e := strconv.ParseInt(match[1], 10, 64); e == nil {
						refs = append(refs, map[string]any{"ref": fmt.Sprintf("node:%d", id)})
					}
				}
				if path := verifiedSpillPointer(plain, outputDir); path != "" {
					addReceiptFile(entry, path)
				}
				if r.IsError {
					entry["diagnostic_preview"] = firstLine(plain, 160)
					entry["occurrences"] = 1
				}
			}
			if r.IsError {
				entry["diagnostic_key"] = fmt.Sprintf("%x", errorHash.Sum(nil))
			}
			if len(refs) > 0 {
				entry["saved_refs"] = refs[:min(12, len(refs))]
			}
			if len(noaRefs) > 0 {
				entry["noa_refs"] = noaRefs
			}
			if len(refs) == 0 && entry["reread"] == nil && entry["output_file"] == nil && entry["ids"] == nil && len(noaRefs) == 0 && !r.IsError {
				recoverable = false
			}
			receipts = append(receipts, entry)
		}
		if !recoverable {
			out = append(out, req.Messages[b.start:b.end]...)
			i = b.end - 1
			continue
		}
		// Retain non-tool answer content. Do not convert historical tool inputs into
		// fake shortened arguments the model might imitate on its next call.
		content := []llm.ContentBlock{}
		for _, c := range req.Messages[i].Content {
			if c.Type != llm.BlockToolUse && c.Type != llm.BlockThinking {
				content = append(content, c)
			}
		}
		if len(content) > 0 {
			copy := req.Messages[i]
			copy.Content = content
			out = append(out, copy)
		}
		raw, _ := json.Marshal(map[string]any{"tool_history_receipts": receipts, "note": "仅为系统生成的旧调用回执索引；不是新执行结果。需要原文时按保存引用重读，或在原始会话按original_call_id查看。"})
		if previous, ok := receiptSegments[len(out)-1]; ok {
			receiptSegments[len(out)-1] = append(previous, receipts...)
		} else {
			receiptSegments[len(out)] = receipts
			out = append(out, llm.UserText(string(raw)))
		}
		i = b.end - 1
	}
	// Share the existing 4K reference-directory budget. Keep recent receipts,
	// merge adjacent batches, and explicitly mark older indices as omitted.
	remaining := retrievalDirectoryRunes - 200
	for _, sys := range req.System {
		if strings.HasPrefix(sys, retrievalDirectoryPrefix) {
			remaining -= utf8.RuneCountInString(sys)
		}
	}
	keep := map[int]bool{}
	truncated := false
	for i := len(out) - 1; i >= 0; i-- {
		entries, ok := receiptSegments[i]
		if !ok {
			keep[i] = true
			continue
		}
		entries = mergeReceiptDiagnostics(entries)
		selected := []map[string]any{}
		for j := len(entries) - 1; j >= 0; j-- {
			candidate := append([]map[string]any{entries[j]}, selected...)
			raw, _ := json.Marshal(map[string]any{"tool_history_receipts": candidate, "older_entries_omitted": j > 0})
			if utf8.RuneCount(raw) > remaining {
				truncated = true
				break
			}
			selected = candidate
		}
		if len(selected) > 0 {
			raw, _ := json.Marshal(map[string]any{"tool_history_receipts": selected, "older_entries_omitted": len(selected) < len(entries)})
			remaining -= utf8.RuneCount(raw)
			out[i] = llm.UserText(string(raw))
			keep[i] = true
		} else {
			truncated = true
		}
	}
	if truncated {
		req.System = append(req.System, "历史工具回执目录已达预算，较早目录项已省略；原始会话和审计仍保留。未展示不表示未执行或没有结果，必要时检索原始节点或执行记录。")
	}
	compact := make([]llm.Message, 0, len(out))
	for i, m := range out {
		if keep[i] {
			compact = append(compact, m)
		}
	}
	req.Messages = compact
	return req
}

// Only identical diagnostics within the same chronological receipt segment are
// combined. Different errors or intervening answers retain separate records.
func mergeReceiptDiagnostics(entries []map[string]any) []map[string]any {
	out := []map[string]any{}
	seen := map[string]int{}
	for _, entry := range entries {
		signature, _ := entry["diagnostic_key"].(string)
		if signature == "" {
			out = append(out, entry)
			continue
		}
		key := fmt.Sprint(entry["tool"]) + ":" + signature
		if pos, ok := seen[key]; ok {
			prior := out[pos]
			prior["occurrences"] = prior["occurrences"].(int) + 1
			prior["last_original_call_id"] = entry["original_call_id"]
		} else {
			seen[key] = len(out)
			out = append(out, entry)
		}
	}
	return out
}

func collectReceiptRefs(v any, out *[]any) {
	if len(*out) >= 12 {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		for _, key := range []string{"ref", "node_id", "finding_node_id", "finding_id", "intent_id", "asset_id"} {
			if value, ok := x[key]; ok {
				if key == "ref" {
					s, ok := value.(string)
					if ok && (strings.HasPrefix(s, "node:") || strings.HasPrefix(s, "activity:")) {
						*out = append(*out, map[string]any{key: s})
					}
				} else if id, ok := structuredInt64(value); ok && id > 0 {
					*out = append(*out, map[string]any{key: id})
				}
			}
		}
		for _, key := range []string{"items", "assets", "members", "parents", "data"} {
			if value, ok := x[key]; ok {
				collectReceiptRefs(value, out)
			}
		}
	case []any:
		for _, item := range x {
			collectReceiptRefs(item, out)
			if len(*out) >= 12 {
				break
			}
		}
	}
}

func projectRequestBudget(ctx context.Context, req llm.CompletionRequest) llm.CompletionRequest {
	ri := RunInfoFrom(ctx)
	if ri.AgentKey != "planner" && ri.AgentKey != "worker" {
		return req
	}
	started := time.Now()
	reads, repeated := 0, 0
	queries := map[string]bool{}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type != llm.BlockToolUse {
				continue
			}
			call := assetContextCall(b)
			if !retrievalTool(call.Name) {
				continue
			}
			reads++
			key := normalizedReadQuery(call.Name, call.Input)
			if key != "" {
				if queries[key] {
					repeated++
				}
				queries[key] = true
			}
		}
	}
	before := measureRequest(req)
	chars, bytes := 32000, 128*1024
	if ri.AgentKey == "worker" {
		chars, bytes = 48000, 192*1024
	}
	if before.Characters > chars || before.Bytes > bytes {
		dir, _ := ctx.Value(contextOutputDirectoryKey{}).(string)
		req = projectCompletedBatchesAt(req, dir)
	}
	after := measureRequest(req)
	log.Printf("[context-input] task=%d role=%s chars=%d bytes=%d system_bytes=%d tool_schema_bytes=%d history_bytes=%d before_chars=%d history_retrieval_calls=%d repeated_queries=%d projection_ms=%d", ri.TaskID, ri.AgentKey, after.Characters, after.Bytes, after.SystemBytes, after.ToolsBytes, after.HistoryBytes, before.Characters, reads, repeated, time.Since(started).Milliseconds())
	if after.Characters <= chars && after.Bytes <= bytes {
		return req
	}
	key := fmt.Sprintf("%d:%s:soft_budget", ri.TaskID, ri.AgentKey)
	now := time.Now()
	budgetWarnings.Lock()
	last := budgetWarnings.items[key]
	warn := now.Sub(last) >= 5*time.Minute
	if warn {
		if len(budgetWarnings.items) >= 1024 {
			for k, t := range budgetWarnings.items {
				if now.Sub(t) > 5*time.Minute {
					delete(budgetWarnings.items, k)
				}
			}
			if len(budgetWarnings.items) >= 1024 {
				for k := range budgetWarnings.items {
					delete(budgetWarnings.items, k)
					break
				}
			}
		}
		budgetWarnings.items[key] = now
	}
	budgetWarnings.Unlock()
	if warn {
		text := fmt.Sprintf("上下文软预算超出：%s 输入%d字符/%d字节，目标%d字符/%d字节。已精简可恢复历史；必要目标和限制保留，按配置继续请求。", ri.AgentKey, after.Characters, after.Bytes, chars, bytes)
		log.Printf("[context-budget-warning] task=%d role=%s chars=%d bytes=%d", ri.TaskID, ri.AgentKey, after.Characters, after.Bytes)
		if emit, ok := ctx.Value(contextObserverKey{}).(func(db.Activity)); ok && emit != nil {
			event := db.Activity{Kind: "warning", Worker: ri.AgentKey, Summary: text, Detail: text}
			if ri.IntentID > 0 {
				event.NodeID = &ri.IntentID
			}
			emit(event)
		}
	}
	return req
}

func logContextUsage(ctx context.Context, usage llm.Usage, elapsed time.Duration, failed bool) {
	ri := RunInfoFrom(ctx)
	if ri.AgentKey != "planner" && ri.AgentKey != "worker" {
		return
	}
	log.Printf("[context-usage] task=%d role=%s input_tokens=%d output_tokens=%d cache_read_tokens=%d cache_write_tokens=%d elapsed_ms=%d failed=%t", ri.TaskID, ri.AgentKey, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheWriteTokens, elapsed.Milliseconds(), failed)
}

var recordedReference = regexp.MustCompile(`(?:fact recorded:|finding recorded:|intent created:|goal added:|hint added:)\s*([0-9]+)`)

func durableWriteTool(name string) bool {
	switch name {
	case "record_fact", "report_finding", "add_intent", "set_goals", "add_hint", "update_finding_report":
		return true
	}
	return false
}

func addReceiptFile(entry map[string]any, path string) {
	if first, ok := entry["output_file"].(string); ok {
		if first == path {
			return
		}
		files, ok := entry["output_files"].([]string)
		if !ok {
			files = []string{first}
		}
		for _, f := range files {
			if f == path {
				return
			}
		}
		entry["output_files"] = append(files, path)
	} else {
		entry["output_file"] = path
	}
}

func historicalResultFailed(call, result llm.ContentBlock) bool {
	if result.IsError || partialReadFailure(normalizedReadQuery(call.Name, call.Input), result) {
		return true
	}
	if !durableWriteTool(call.Name) {
		return false
	}
	for _, c := range result.Content {
		if c.Type != llm.BlockText {
			continue
		}
		var value map[string]any
		if decodeStructuredJSON([]byte(noaadapter.StripRefTag(c.Text)), &value) != nil {
			continue
		}
		if rows, ok := value["errors"].([]any); ok {
			for _, e := range rows {
				if e != nil && e != "" && e != false {
					return true
				}
			}
		}
	}
	return false
}

// Output text alone cannot prove that a file exists. Accept only the official
// Capture pointer in this run's host-configured output directory, with matching
// length and head bytes. Arbitrary application JSON IDs are never graph refs.
func verifiedSpillPointer(text, dir string) string {
	if dir == "" {
		return ""
	}
	marker := "Full output saved to  "
	pos := strings.LastIndex(text, marker)
	if pos < 0 {
		return ""
	}
	path, end, ok := strings.Cut(text[pos+len(marker):], " </persisted-output>")
	if !ok || end != "" || len(path) > 4096 || !filepath.IsAbs(path) {
		return ""
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Dir(resolved) != root {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	start := strings.LastIndex(text[:pos], "... <persisted-output>[Output too large: full ")
	if start < 256 {
		return ""
	}
	var size int64
	if n, e := fmt.Sscanf(text[start:], "... <persisted-output>[Output too large: full %d bytes", &size); e != nil || n != 1 || size != info.Size() {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return ""
	}
	head := make([]byte, 256)
	if _, err = io.ReadFull(file, head); err != nil || string(head) != text[:256] {
		return ""
	}
	return path
}
