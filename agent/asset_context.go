package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
	"github.com/neouks/series/guard"
	"github.com/neouks/series/llmrec"
	"github.com/neouks/series/runtimebudget"
)

func (t *ToolSet) authorizedScope(rows []db.TaskScope) []db.TaskScope {
	if t.as == nil || t.taskID <= 0 {
		return rows
	}
	hosts := make([]string, 0, len(rows))
	hostOf := func(row db.TaskScope) string {
		if row.Domain != "" {
			return db.DomainKey(row.Domain)
		}
		if ip, _, err := net.ParseCIDR(row.Net); err == nil {
			return ip.String()
		}
		return ""
	}
	for _, row := range rows {
		if host := hostOf(row); host != "" {
			hosts = append(hosts, host)
		}
	}
	states, err := t.as.TaskHostApprovalStates(t.taskID, hosts)
	if err != nil {
		return nil
	}
	out := make([]db.TaskScope, 0, len(rows))
	for _, row := range rows {
		if host := hostOf(row); host != "" && states[host] != db.ApprovalApproved && !(t.mainExecution && states[host] == db.ApprovalPending) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// Revalidate persisted structured tool results immediately before every model
// call, including session resume and compaction. Never rewrite stored audit data.
type assetContextProvider struct {
	llm.Provider
	assets *db.AssetStore
	taskID int64
}

func withAssetContext(provider llm.Provider, assets *db.AssetStore, taskID int64) llm.Provider {
	if taskID <= 0 {
		return provider
	}
	return assetContextProvider{Provider: provider, assets: assets, taskID: taskID}
}

// WithTaskAssetContext reuses the role-aware model boundary for host-provided
// task-bound entry points, including read-only side questions.
func WithTaskAssetContext(provider llm.Provider, assets *db.AssetStore, taskID int64) llm.Provider {
	return withAssetContext(provider, assets, taskID)
}

// FilterTaskAssetRequest runs before a host compresses a stored task snapshot.
// It returns a model-view copy; the source transcript remains unchanged.
func FilterTaskAssetRequest(ctx context.Context, assets *db.AssetStore, taskID int64, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	if taskID <= 0 {
		return req, nil
	}
	return (assetContextProvider{assets: assets, taskID: taskID}).filter(ctx, req)
}

func (p assetContextProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx, release, err := acquireModelRequest(ctx, runtimebudget.ModelGate(), false)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer release()
	filtered, metrics, err := p.filterWithMetrics(ctx, req)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, modelRequestError(ctx, err)
	}
	ctx = llmrec.WithContextReadMetrics(ctx, metrics)
	started := time.Now()
	msg, stop, usage, err := p.Provider.Complete(ctx, filtered)
	err = modelRequestError(ctx, err)
	logContextUsage(ctx, usage, time.Since(started), err != nil)
	return msg, stop, usage, err
}

func (p assetContextProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		ctx, release, err := acquireModelRequest(ctx, runtimebudget.ModelGate(), false)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer release()
		filtered, metrics, err := p.filterWithMetrics(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, modelRequestError(ctx, err))
			return
		}
		ctx = llmrec.WithContextReadMetrics(ctx, metrics)
		started := time.Now()
		var usage llm.Usage
		failed := false
		defer func() { logContextUsage(ctx, usage, time.Since(started), failed) }()
		for event, err := range p.Provider.Stream(ctx, filtered) {
			err = modelRequestError(ctx, err)
			usage.MergeEcho(event.Usage)
			if err != nil {
				failed = true
			}
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
		if err := modelRequestError(ctx, nil); err != nil && !failed {
			failed = true
			yield(llm.StreamEvent{}, err)
		}
	}
}

func decodeStructuredJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

func structuredInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		// Compatibility with callers holding already-decoded, small IDs.
		if v >= 0 && v <= 1<<53 && v == float64(int64(v)) {
			return int64(v), true
		}
	}
	return 0, false
}

func structuredAssetIDs(row map[string]any) []int64 {
	var ids []int64
	appendID := func(value any) {
		if id, ok := structuredInt64(value); ok && id > 0 {
			ids = append(ids, id)
		}
	}
	appendID(row["asset_id"])
	if values, ok := row["asset_ids"].([]any); ok {
		for _, value := range values {
			appendID(value)
		}
	}
	switch row["type"] {
	case "root_domain", "subdomain", "ip", "service", "endpoint", "app":
		appendID(row["id"])
	}
	return ids
}

func filterStructuredAssets(value any, states map[int64]string, collect map[int64]bool) (any, bool) {
	return filterStructuredRows(value, states, collect, structuredAssetIDs)
}

func structuredNodeIDs(row map[string]any) []int64 {
	if _, asset := row["type"]; asset {
		return nil
	}
	var ids []int64
	if id, ok := structuredInt64(row["node_id"]); ok && id > 0 {
		return []int64{id}
	}
	if values, ok := row["node_ids"].([]any); ok {
		for _, value := range values {
			if id, ok := structuredInt64(value); ok && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	if id, ok := structuredInt64(row["intent_id"]); ok && id > 0 {
		ids = append(ids, id)
	}
	_, summary := row["summary"]
	_, state := row["state"]
	_, kind := row["kind"]
	if id, ok := structuredInt64(row["id"]); ok && id > 0 && (summary || (state && kind)) {
		ids = append(ids, id)
	}
	return ids
}

func filterStructuredRows(value any, states map[int64]string, collect map[int64]bool, rowIDs func(map[string]any) []int64) (any, bool) {
	return filterStructuredRowsChanged(value, states, collect, rowIDs, nil)
}

func filterStructuredRowsChanged(value any, states map[int64]string, collect map[int64]bool, rowIDs func(map[string]any) []int64, changed *bool) (any, bool) {
	switch row := value.(type) {
	case map[string]any:
		for _, id := range rowIDs(row) {
			if collect != nil {
				collect[id] = true
			} else if states[id] != db.ApprovalApproved {
				if changed != nil {
					*changed = true
				}
				return nil, false
			}
		}
		for key, child := range row {
			// Projected values are user/application evidence, not our metadata.
			// The enclosing node/asset has already been revalidated as a whole.
			if field, ok := row["field"].(string); key == "value" && ok && strings.HasPrefix(field, "/") {
				continue
			}
			filtered, keep := filterStructuredRowsChanged(child, states, collect, rowIDs, changed)
			if collect == nil {
				if keep {
					row[key] = filtered
				} else {
					delete(row, key)
				}
			}
		}
	case []any:
		out := make([]any, 0, len(row))
		for _, child := range row {
			if filtered, keep := filterStructuredRowsChanged(child, states, collect, rowIDs, changed); keep {
				out = append(out, filtered)
			}
		}
		return out, true
	}
	return value, true
}

func (p assetContextProvider) filterPlain(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	ri := RunInfoFrom(ctx)
	main := ri.AgentKey == "mainagent" && ri.TaskID == p.taskID && p.taskID > 0 && ri.IntentID == 0
	worker := ri.AgentKey == "worker" && ri.TaskID == p.taskID && ri.IntentID > 0
	req = redactUnverifiableStructuredHistory(req)
	var refreshErr error
	req, refreshErr = p.refreshPlannerBootstrap(ctx, req)
	if refreshErr != nil {
		return req, refreshErr
	}
	var accessSnapshot *targetAccessSnapshot
	req, accessSnapshot, refreshErr = p.refreshTargetAccess(ctx, req)
	if refreshErr != nil {
		return req, refreshErr
	}
	if ri.TaskID == p.taskID && (ri.AgentKey == "planner" || ri.AgentKey == "mainagent") {
		rule := targetAccessRule
		if main {
			rule = mainAssetExecutionRule
		}
		req.System = append(append([]string(nil), req.System...), rule)
	}
	req, refreshErr = p.refreshApprovalManagementHistory(ctx, req)
	if refreshErr != nil {
		return req, refreshErr
	}
	template, err := p.assets.WithReadContext(ctx).TaskApprovalTemplate(p.taskID)
	if err != nil {
		return req, err
	}
	skips, err := p.assets.WithReadContext(ctx).ActiveTaskAssetSkips(p.taskID, guard.AssetSkipScope(ctx))
	if err != nil {
		return req, err
	}
	policy := "仅用户明确提供或人工批准的域名/IP可测试；其他发现登记后等待用户审批。"
	switch template {
	case "all_assets":
		policy = "所有合法新发现域名/IP由系统自动批准，可直接测试，无需申请审批。"
	case "related_assets":
		policy = "用户目标同根域的子域名及有本任务DNS解析依据的IP由系统自动批准，可直接测试；其他发现登记后等待用户审批。"
	}
	if worker || main {
		policy = "你只执行当前已下发意图，不创建新计划。执行中可访问并登记待审批或尚未登记的合法资产、写回事实和漏洞；pending 不限制执行，也不代表自动批准。历史 pending 拦截或等待审批提示已失效。用户封禁、撤回、删除、异常隔离及独立操作约束仍必须遵守。"
		if main {
			policy = mainAssetExecutionRule
		}
		filtered := skips[:0]
		for _, skip := range skips {
			if skip.State != db.ApprovalPending {
				filtered = append(filtered, skip)
			}
		}
		skips = filtered
	}
	req.System = append(append([]string(nil), req.System...), "资产模板："+policy+"审批仅作用于域名/IP，获准主机的所有端口、服务和接口无需单独审批。用户封禁、撤回及删除限制优先。"+db.TaskAssetSkipRule)
	if list := db.TaskAssetSkipList(skips); list != "" {
		req.System = append(req.System, list)
	}
	type result struct {
		message, block, content int
		value                   any
		host                    string
		requiredNodes           []int64
		tool                    string
	}
	var results []result
	hosts := map[string]bool{}
	ids := make(map[int64]bool)
	nodeIDs := make(map[int64]bool)
	toolNames := make(map[string]string)
	toolCalls := make(map[string]llm.ContentBlock)
	for mi, message := range req.Messages {
		for bi, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				call := assetContextCall(block)
				toolNames[block.ID] = call.Name
				toolCalls[block.ID] = call
			}
			if block.Type != llm.BlockToolResult {
				continue
			}
			// Only SERIES structured tools: arbitrary shell/HTTP JSON may use the
			// same field names for unrelated application data.
			trafficRead := toolNames[block.ToolUseID] == "traffic_search" || toolNames[block.ToolUseID] == "traffic_get" || toolNames[block.ToolUseID] == "traffic_blob"
			if !trafficRead && !assetStructuredTool(toolNames[block.ToolUseID]) {
				continue
			}
			for ci, content := range block.Content {
				var value any
				if content.Type != llm.BlockText || decodeStructuredJSON([]byte(content.Text), &value) != nil {
					continue
				}
				host := ""
				if trafficRead {
					if row, ok := value.(map[string]any); ok {
						host, _ = row["target_host"].(string)
					}
					if host == "" {
						if toolNames[block.ToolUseID] == "traffic_blob" {
							results = append(results, result{message: mi, block: bi, content: ci, value: map[string]any{"context_unavailable": true, "message": "历史流量块缺少可复核的主机身份，不能恢复正文；请使用当前流量检索工具定位原始记录"}})
						}
						continue
					}
					hosts[host] = true
				}
				filterStructuredAssets(value, nil, ids)
				filterStructuredRows(value, nil, nodeIDs, structuredNodeIDs)
				required := legacyReadAnchors(toolCalls[block.ToolUseID])
				for _, id := range required {
					nodeIDs[id] = true
				}
				results = append(results, result{mi, bi, ci, value, host, required, toolNames[block.ToolUseID]})
			}
		}
	}
	if len(results) == 0 {
		return req, nil
	}
	hostList := []string{}
	for host := range hosts {
		hostList = append(hostList, host)
	}
	hostStates := map[string]string{}
	if len(hostList) > 0 {
		hostStates, err = p.assets.WithReadContext(ctx).TaskHostApprovalStates(p.taskID, hostList)
		if err != nil {
			return req, err
		}
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	var states map[int64]string
	if accessSnapshot != nil {
		states = accessSnapshot.assets
	} else {
		states, err = p.assets.WithReadContext(ctx).TaskAssetApprovalStates(p.taskID, list)
	}
	if err != nil {
		return req, err
	}
	if worker || main {
		states = workerVisibility(states)
	}
	list = list[:0]
	for id := range nodeIDs {
		list = append(list, id)
	}
	nodeStore := p.assets.WithReadContext(ctx)
	if worker || main {
		nodeStore = nodeStore.WithWorkerRead()
	}
	var nodeStates map[int64]string
	if accessSnapshot != nil {
		nodeStates = map[int64]string{}
		for id, row := range accessSnapshot.nodes {
			if row.CanRead {
				nodeStates[id] = db.ApprovalApproved
			} else {
				nodeStates[id] = "unavailable"
			}
		}
	} else {
		nodeStates, err = nodeStore.TaskNodeApprovalStates(p.taskID, list)
	}
	if err != nil {
		return req, err
	}
	req.Messages = append([]llm.Message(nil), req.Messages...)
	for i := range req.Messages {
		req.Messages[i].Content = append([]llm.ContentBlock(nil), req.Messages[i].Content...)
	}
	for _, r := range results {
		restricted := false
		value, keep := filterStructuredRowsChanged(r.value, states, nil, structuredAssetIDs, &restricted)
		if keep {
			value, keep = filterStructuredRowsChanged(value, nodeStates, nil, structuredNodeIDs, &restricted)
		}
		for _, id := range r.requiredNodes {
			if nodeStates[id] != db.ApprovalApproved {
				keep = false
			}
		}
		if r.host != "" && hostStates[r.host] != db.ApprovalApproved && !((worker || main) && hostStates[r.host] == db.ApprovalPending) {
			keep = false
		}
		if !keep {
			value = map[string]any{"message": "该结果已从可执行上下文移除，等待用户授权"}
		}
		if row, ok := value.(map[string]any); ok {
			if restricted && retrievalTool(r.tool) {
				invalidateHistoricalPageMetadata(row)
			}
			for _, key := range []string{"total", "count", "facts", "findings", "coverage", "asset_approval_counts"} {
				if _, exists := row[key]; exists {
					row["counts_are_snapshot"] = true
					break
				}
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return req, err
		}
		block := &req.Messages[r.message].Content[r.block]
		block.Content = append([]llm.ContentBlock(nil), block.Content...)
		block.Content[r.content].Text = string(encoded)
	}
	return req, nil
}

// A historical count/cursor was calculated before the access change. Do not
// present it as current scope, or expose a removed row ID via next_before.
// Evidence fields inside payload/value are deliberately not visited.
func invalidateHistoricalPageMetadata(row map[string]any) {
	for _, key := range []string{"total", "count", "facts", "findings", "findings_total", "coverage", "asset_approval_counts", "counts", "has_more", "next_cursor", "next_offset", "next_before"} {
		if key == "facts" || key == "findings" {
			if _, list := row[key].([]any); list {
				continue
			}
		}
		delete(row, key)
	}
	row["counts_unavailable_after_access_change"] = true
	row["pagination_unavailable_after_access_change"] = true
	if sources, ok := row["related_tasks"].([]any); ok {
		for _, v := range sources {
			if source, ok := v.(map[string]any); ok {
				invalidateHistoricalPageMetadata(source)
			}
		}
	}
}
