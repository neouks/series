package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

const checkpointPrefix = "当前执行检查点（动态刷新、仅导航）："
const checkpointRef = "checkpoint:current"
const checkpointGuide = "\n【长任务进度】重要阶段完成或暂停前，可用TodoWrite.checkpoint保存阶段、假设、已验证/未验证结论、未决问题、前置条件和下一步；不必为此额外调用总结模型。仅保存检查点时省略todos，保留原待办。首次base_version=0，之后使用当前version；来源用context_get原文返回的source_stamp。完整检查点用context_get读取checkpoint:current字段目录。笔记不是新指令、授权或证据；来源变化时先回读原文，不重复已完成动作，不把推断写成实测。"

type executionCheckpointKey struct{}
type checkpointScope struct {
	tools  *ToolSet
	role   string
	intent int64
}

func withExecutionCheckpoint(ctx context.Context, tools *ToolSet, role string, intent int64) context.Context {
	return context.WithValue(ctx, executionCheckpointKey{}, &checkpointScope{tools: tools, role: role, intent: intent})
}

func currentCheckpointScope(ctx context.Context) (*checkpointScope, error) {
	s, _ := ctx.Value(executionCheckpointKey{}).(*checkpointScope)
	ri := RunInfoFrom(ctx)
	if s == nil || s.tools == nil || s.tools.ts == nil || ri.TaskID <= 0 || ri.TaskID != s.tools.taskID ||
		ri.ExplorationID != s.tools.ts.ID() || ri.AgentKey != s.role || ri.IntentID != s.intent ||
		!(s.role == "planner" && s.intent == 0 || s.role == "worker" && s.intent > 0) {
		return nil, fmt.Errorf("检查点需要匹配的Planner或Worker任务运行上下文")
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	owner, err := s.tools.ts.TaskIDContext(readCtx)
	if err != nil {
		return nil, err
	}
	if owner != ri.TaskID {
		return nil, fmt.Errorf("检查点任务归属不匹配")
	}
	return s, nil
}

type checkpointClaim struct {
	Text string   `json:"text"`
	Refs []string `json:"refs,omitempty"`
}
type checkpointBody struct {
	Stage         string                `json:"stage"`
	Goal          string                `json:"goal"`
	Hypotheses    []checkpointClaim     `json:"hypotheses,omitempty"`
	Verified      []checkpointClaim     `json:"verified,omitempty"`
	Unverified    []checkpointClaim     `json:"unverified,omitempty"`
	Unresolved    []checkpointClaim     `json:"unresolved,omitempty"`
	Prerequisites []checkpointClaim     `json:"prerequisites,omitempty"`
	NextSteps     []checkpointClaim     `json:"next_steps,omitempty"`
	Sources       []db.CheckpointSource `json:"sources,omitempty"`
}
type checkpointInput struct {
	BaseVersion int64 `json:"base_version"`
	checkpointBody
}

func checkpointSchema() map[string]any {
	claim := obj(map[string]any{
		"text": map[string]any{"type": "string", "maxLength": 300},
		"refs": map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"}},
	}, "text")
	properties := map[string]any{
		"base_version": map[string]any{"type": "integer", "minimum": 0, "description": "当前检查点version；首次0，冲突时读取checkpoint:current"},
		"stage":        str("当前阶段，最多120字符"), "goal": str("当前工作目标，最多500字符；不替代原始任务目标"),
		"sources": map[string]any{"type": "array", "maxItems": 16, "items": obj(map[string]any{"ref": str("node:ID"), "stamp": str("原文context_get返回的source_stamp，不能猜测或改写")}, "ref", "stamp")},
	}
	for _, field := range []string{"hypotheses", "verified", "unverified", "unresolved", "prerequisites", "next_steps"} {
		properties[field] = map[string]any{"type": "array", "maxItems": 6, "items": claim}
	}
	return obj(properties, "base_version", "stage", "goal")
}

type checkpointTodoTool struct {
	actool.CoreTool
}

func (t checkpointTodoTool) InputSchema() map[string]any {
	schema := normalizeToolSchema(t.CoreTool.InputSchema())
	props, _ := schema["properties"].(map[string]any)
	props["checkpoint"] = checkpointSchema()
	schema["required"] = []string{}
	return schema
}

func (t checkpointTodoTool) Description() string {
	return t.CoreTool.Description() + " 可选checkpoint保存阶段/假设/已验证与未验证结论/未决问题/前置条件/下一步，总计6000字符。todos与checkpoint至少一个；省略todos保留完整清单。refs须在sources中，来源stamp取context_get的source_stamp；verified仅引用fact/finding。检查点只是Agent笔记，关键判断须回读原始证据，不能凭它授权或认定目标达成。"
}

func (t checkpointTodoTool) Prompt() string {
	return "todos提供时完整替换清单；checkpoint提供时完整替换自身阶段笔记，省略另一项则保留。至少提供一项。checkpoint使用base_version和原文source_stamp；不要复制报文，不把摘要或推断当证据。"
}

func checkpointActivityMetadata(call llm.ContentBlock) json.RawMessage {
	call = assetContextCall(call)
	if call.Name == "TodoWrite" {
		var input map[string]json.RawMessage
		if json.Unmarshal(call.Input, &input) == nil && input["checkpoint"] != nil {
			return json.RawMessage(`{"checkpoint_note":true}`)
		}
	}
	if call.Name == "context_get" {
		var input struct {
			Items []contextRead `json:"items"`
		}
		if json.Unmarshal(call.Input, &input) == nil && slices.ContainsFunc(input.Items, func(item contextRead) bool { return strings.HasPrefix(item.Ref, "checkpoint:") }) {
			return json.RawMessage(`{"checkpoint_note":true}`)
		}
	}
	return nil
}

func decodeCheckpoint(raw []byte) (checkpointInput, error) {
	var in checkpointInput
	if len(raw) > 65536 {
		return in, fmt.Errorf("检查点输入超过64KiB，请保存简短进度和引用")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return in, fmt.Errorf("检查点仅接受一个JSON对象")
	}
	if in.BaseVersion < 0 || in.BaseVersion == int64(^uint64(0)>>1) || strings.TrimSpace(in.Stage) == "" || strings.TrimSpace(in.Goal) == "" || utf8.RuneCountInString(in.Stage) > 120 || utf8.RuneCountInString(in.Goal) > 500 || len(in.Sources) > 16 {
		return in, fmt.Errorf("检查点阶段/目标/版本或来源数量无效")
	}
	sources := map[string]bool{}
	for _, source := range in.Sources {
		kind, id, err := parseContextRef(source.Ref)
		if err != nil || kind != "node" || source.Ref != fmt.Sprintf("node:%d", id) || len(source.Stamp) != 32 || source.Kind != "" || sources[source.Ref] {
			return in, fmt.Errorf("检查点来源须为不重复的node引用及原始source_stamp")
		}
		sources[source.Ref] = true
	}
	for _, group := range [][]checkpointClaim{in.Hypotheses, in.Verified, in.Unverified, in.Unresolved, in.Prerequisites, in.NextSteps} {
		if len(group) > 6 {
			return in, fmt.Errorf("每类检查点条目最多6条")
		}
		for _, item := range group {
			if strings.TrimSpace(item.Text) == "" || utf8.RuneCountInString(item.Text) > 300 || len(item.Refs) > 16 {
				return in, fmt.Errorf("检查点说明最多300字符、16个引用")
			}
			for _, ref := range item.Refs {
				if !sources[ref] {
					return in, fmt.Errorf("结论引用必须包含在sources中")
				}
			}
		}
	}
	for _, item := range in.Verified {
		if len(item.Refs) == 0 {
			return in, fmt.Errorf("verified必须引用已读取的原始事实或漏洞")
		}
	}
	body, _ := json.Marshal(in.checkpointBody)
	if utf8.RuneCount(body) > 6000 || len(body) > 32768 {
		return in, fmt.Errorf("检查点正文总计最多6000字符/32768字节，请保留引用，勿复制证据正文")
	}
	return in, nil
}

func (s *checkpointScope) sourceSnapshot(ctx context.Context, sources []db.CheckpointSource) (db.CheckpointSources, error) {
	if !s.allowsDetail(ctx) {
		return db.CheckpointSources{}, fmt.Errorf("当前角色未启用上下文详情读取，不能保存来源笔记")
	}
	target, err := s.tools.contextTarget(ctx, "", "detail")
	if err != nil {
		return db.CheckpointSources{}, err
	}
	ids := make([]int64, 0, len(sources))
	for _, source := range sources {
		kind, id, e := parseContextRef(source.Ref)
		if e != nil || kind != "node" {
			return db.CheckpointSources{}, fmt.Errorf("无效检查点来源")
		}
		ids = append(ids, id)
	}
	ownerAdded := s.role == "worker" && !slices.Contains(ids, s.intent)
	if ownerAdded {
		ids = append(ids, s.intent)
	}
	snapshot, err := target.ts.CheckpointSourceSnapshot(ctx, ids, s.intent)
	if err != nil {
		return snapshot, err
	}
	if s.role == "worker" {
		index := slices.IndexFunc(snapshot.Sources, func(source db.CheckpointSource) bool {
			return source.Ref == fmt.Sprintf("node:%d", s.intent) && source.Kind == "intent"
		})
		if index < 0 {
			return snapshot, fmt.Errorf("当前Worker意图已删除或不可访问")
		}
		if snapshot.OwnerStamp == "" {
			return snapshot, fmt.Errorf("当前Worker意图归属不匹配")
		}
		snapshot.ContextStamp += "|owner:" + snapshot.OwnerStamp
		if ownerAdded {
			snapshot.Sources = slices.Delete(snapshot.Sources, index, index+1)
		}
	}
	return snapshot, nil
}

func (s *checkpointScope) allowsDetail(ctx context.Context) bool {
	return contextAllows(ctx, "detail", false) && (s.tools.checkpointReadAccess == nil || slices.Contains(s.tools.checkpointReadAccess.Local, "detail"))
}

func sameCheckpointSources(want, got []db.CheckpointSource) bool {
	if len(want) != len(got) {
		return false
	}
	for _, source := range want {
		if !slices.ContainsFunc(got, func(current db.CheckpointSource) bool {
			return current.Ref == source.Ref && current.Stamp == source.Stamp
		}) {
			return false
		}
	}
	return true
}

func (t checkpointTodoTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	scope, err := currentCheckpointScope(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if err = actool.ValidateInput(t.InputSchema(), input); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(input, &fields); err != nil {
		return actool.Errorf("无效进度参数"), nil
	}
	for field := range fields {
		if field != "todos" && field != "checkpoint" {
			return actool.Errorf("未知进度参数：" + field), nil
		}
	}
	if len(fields) == 0 {
		return actool.Errorf("todos与checkpoint至少提供一个"), nil
	}
	var cp checkpointInput
	var snapshot db.CheckpointSources
	if raw, ok := fields["checkpoint"]; ok {
		cp, err = decodeCheckpoint(raw)
		if err == nil {
			snapshot, err = scope.sourceSnapshot(ctx, cp.Sources)
		}
		if err == nil && !sameCheckpointSources(cp.Sources, snapshot.Sources) {
			err = fmt.Errorf("检查点来源已变化或不可访问；回读原文，不能将旧结论贴成新版本")
		}
		if err == nil {
			for _, claim := range cp.Verified {
				for _, ref := range claim.Refs {
					if !slices.ContainsFunc(snapshot.Sources, func(source db.CheckpointSource) bool {
						return source.Ref == ref && (source.Kind == "fact" || source.Kind == "finding")
					}) {
						err = fmt.Errorf("verified只能引用原始fact/finding，摘要或待办不是证据")
					}
				}
			}
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
	}
	var result actool.Result
	ack := map[string]any{}
	if todos, ok := fields["todos"]; ok {
		if bytes.Equal(bytes.TrimSpace(todos), []byte("null")) {
			return actool.Errorf("todos不能为null；清空请提供[]"), nil
		}
		coreInput, _ := json.Marshal(map[string]json.RawMessage{"todos": todos})
		result, err = t.CoreTool.Call(ctx, coreInput, tc)
		if err != nil || result.IsError {
			return result, err
		}
		if json.Unmarshal([]byte(result.Flatten()), &ack) != nil {
			ack["message"] = result.Flatten()
		}
	}
	if _, ok := fields["checkpoint"]; !ok {
		return result, nil
	}
	body, _ := json.Marshal(cp.checkpointBody)
	version, err := scope.tools.ts.SaveExecutionCheckpoint(ctx, scope.role, scope.intent, cp.BaseVersion, body, snapshot)
	if err != nil {
		_, todosStored := fields["todos"]
		out, _ := jsonResult(map[string]any{"error": err.Error(), "todos_stored": todosStored, "checkpoint_stored": false, "read": map[string]any{"items": []contextRead{{Ref: checkpointRef}}}})
		out.IsError = true
		return out, nil
	}
	ack["stored"], ack["checkpoint_ref"], ack["version"] = true, checkpointRef, version
	ack["navigation_only"], ack["todos_updated"] = true, fields["todos"] != nil
	return jsonResult(ack)
}

func checkpointStatus(ctx context.Context) (map[string]any, *checkpointBody, error) {
	scope, err := currentCheckpointScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	row, err := scope.tools.ts.ExecutionCheckpoint(ctx, scope.role, scope.intent)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]any{"ref": checkpointRef, "owner_role": scope.role, "intent_id": scope.intent, "navigation_only": true, "version": int64(0), "status": "absent"}
	if row == nil {
		return out, nil, nil
	}
	out["version"], out["saved_at"] = row.Version, row.SavedAt
	if !scope.allowsDetail(ctx) {
		out["status"] = "unavailable"
		out["message"] = "当前角色没有上下文详情权限；检查点正文未展示。"
		return out, nil, nil
	}
	var body checkpointBody
	decoder := json.NewDecoder(bytes.NewReader(row.Body))
	decoder.DisallowUnknownFields()
	if len(row.Body) > 32768 || decoder.Decode(&body) != nil {
		return nil, nil, fmt.Errorf("检查点内容损坏，不能按空状态继续")
	}
	validated, _ := json.Marshal(checkpointInput{checkpointBody: body})
	if _, err = decodeCheckpoint(validated); err != nil || !sameCheckpointSources(body.Sources, row.Sources) {
		return nil, nil, fmt.Errorf("检查点内容或来源元数据损坏，不能按空状态继续")
	}
	snapshot, err := scope.sourceSnapshot(ctx, row.Sources)
	if err != nil {
		return nil, nil, err
	}
	if row.ContextStamp != snapshot.ContextStamp || !sameCheckpointSources(row.Sources, snapshot.Sources) {
		out["status"] = "stale_or_unavailable"
		out["message"] = "来源或任务要求已变化；旧笔记不代表当前结论，自动引导不注入其正文。先回读原始证据再更新，不能把旧verified当当前验证。"
		directory := []map[string]any{}
		allReadable := len(row.Sources) == len(snapshot.Sources)
		for _, source := range row.Sources {
			index := slices.IndexFunc(snapshot.Sources, func(current db.CheckpointSource) bool { return current.Ref == source.Ref })
			if index < 0 {
				allReadable = false
				// Echo only the checkpoint's already-known typed ID. The normal
				// reader can distinguish stale-summary navigation from denial;
				// no inaccessible kind, task, stamp or body is exposed here.
				directory = append(directory, map[string]any{"ref": source.Ref, "status": "unavailable_or_stale", "read": map[string]any{"items": []contextRead{{Ref: source.Ref}}}})
				continue
			}
			directory = append(directory, map[string]any{"ref": source.Ref, "changed": snapshot.Sources[index].Stamp != source.Stamp, "read": map[string]any{"items": []contextRead{{Ref: source.Ref}}}})
		}
		out["sources"] = directory
		out["can_read_prior_notes"] = allReadable
		if allReadable {
			// Explicitly requested old notes remain inspectable only while ALL
			// provenance is readable; they are never a freshly validated claim.
			return out, &body, nil
		}
		return out, nil, nil
	}
	out["status"] = "current"
	out["permissions_rechecked"] = row.AccessRevision != snapshot.AccessRevision
	return out, &body, nil
}

func (t *ToolSet) readExecutionCheckpoint(ctx context.Context, task string, item contextRead, budget int) (map[string]any, error) {
	expected := int64(0)
	if item.Ref != checkpointRef {
		suffix, ok := strings.CutPrefix(item.Ref, checkpointRef+"@")
		var err error
		expected, err = strconv.ParseInt(suffix, 10, 64)
		if !ok || err != nil || expected <= 0 || suffix != strconv.FormatInt(expected, 10) {
			return nil, fmt.Errorf("无效检查点引用；首次读取使用checkpoint:current")
		}
	}
	scope, err := currentCheckpointScope(ctx)
	if err != nil || scope.tools != t || task != "" && task != strconv.FormatInt(t.taskID, 10) {
		return nil, fmt.Errorf("检查点仅允许当前Planner/Worker读取自身状态，不支持跨任务")
	}
	if item.View != "" && item.View != "detail" || item.Before != 0 || item.Offset < 0 || item.Index < 0 {
		return nil, fmt.Errorf("检查点仅支持detail、field/index/offset")
	}
	out, body, err := checkpointStatus(ctx)
	if err != nil {
		return out, err
	}
	version, _ := out["version"].(int64)
	if expected > 0 && expected != version {
		return nil, fmt.Errorf("检查点版本已变化；请从checkpoint:current重新读取，不能拼接不同版本")
	}
	out["ref"] = item.Ref
	if body == nil {
		return out, nil
	}
	pageRef := fmt.Sprintf("%s@%d", checkpointRef, version)
	raw, _ := json.Marshal(body)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	if item.Field == "" {
		keys := []string{}
		for key := range fields {
			keys = append(keys, "/"+key)
		}
		slices.Sort(keys)
		if item.Offset != 0 || item.Index > len(keys) {
			return nil, fmt.Errorf("检查点字段目录位置越界")
		}
		end := min(len(keys), item.Index+max(1, budget/100))
		out["fields"] = keys[item.Index:end]
		if end < len(keys) {
			out["next"] = map[string]any{"items": []contextRead{{Ref: pageRef, Index: end}}}
		}
		return out, nil
	}
	value, exists := fields[strings.TrimPrefix(item.Field, "/")]
	if !strings.HasPrefix(item.Field, "/") || !exists {
		return nil, fmt.Errorf("检查点字段不存在，请先读字段目录")
	}
	var text string
	if json.Unmarshal(value, &text) != nil {
		text = string(value) // Field JSON is explicitly textual, not a partial JSON value.
	}
	runes := []rune(text)
	if item.Index != 0 || item.Offset > len(runes) {
		return nil, fmt.Errorf("检查点文本位置越界")
	}
	end := min(len(runes), item.Offset+max(1, budget-300))
	out["field"], out["text"], out["offset"], out["has_more"] = item.Field, string(runes[item.Offset:end]), item.Offset, end < len(runes)
	if end < len(runes) {
		out["next"] = map[string]any{"items": []contextRead{{Ref: pageRef, Field: item.Field, Offset: end}}}
	}
	return out, nil
}

// Refresh from durable state on every request; no model-generated summary is
// required and no historical audit/SDK messages are edited in place.
func refreshExecutionCheckpoint(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	if _, ok := ctx.Value(executionCheckpointKey{}).(*checkpointScope); !ok {
		return req, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, body, err := checkpointStatus(ctx)
	if err != nil {
		return req, fmt.Errorf("读取执行检查点失败: %w", err)
	}
	req = withoutSystemSegments(req, func(s string) bool { return strings.HasPrefix(s, checkpointPrefix) })
	// Old explicit reads cannot bypass current source/role/intent freshness.
	cloned := false
	calls := map[string]string{}
	for mi, message := range req.Messages {
		for bi, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				calls[block.ID] = assetContextCall(block).Name
			}
			if block.Type == llm.BlockToolUse && assetContextCall(block).Name == "TodoWrite" {
				// The durable checkpoint, revalidated above, is the current view.
				// Keeping its old write arguments would leak stale source claims
				// even after the matching read/result body had been filtered.
				call := assetContextCall(block)
				var params map[string]json.RawMessage
				if json.Unmarshal(call.Input, &params) == nil && params["checkpoint"] != nil {
					params["checkpoint"] = json.RawMessage(`{"history_omitted":true,"ref":"checkpoint:current","original_audit_retained":true,"navigation_only":true}`)
					updated, _ := json.Marshal(params)
					if block.Name == actool.ExecuteExtraToolName {
						var outer map[string]json.RawMessage
						if json.Unmarshal(block.Input, &outer) == nil {
							outer["params"] = updated
							updated, _ = json.Marshal(outer)
						}
					}
					if !cloned {
						req = cloneToolMessages(req)
						cloned = true
					}
					req.Messages[mi].Content[bi].Input = updated
				}
			}
			if block.Type != llm.BlockToolResult || calls[block.ToolUseID] != "context_get" {
				continue
			}
			for ci, part := range block.Content {
				var value map[string]any
				if part.Type != llm.BlockText || decodeStructuredJSON([]byte(part.Text), &value) != nil {
					continue
				}
				items, _ := value["items"].([]any)
				changed := false
				for i, item := range items {
					row, ok := item.(map[string]any)
					ref, _ := row["ref"].(string)
					if !ok || ref != checkpointRef && !strings.HasPrefix(ref, checkpointRef+"@") {
						continue
					}
					version, _ := structuredInt64(row["version"])
					intent, _ := structuredInt64(row["intent_id"])
					readable := status["status"] == "current" || status["can_read_prior_notes"] == true && row["status"] == status["status"] && row["can_read_prior_notes"] == true
					if !readable || version != status["version"] || row["owner_role"] != status["owner_role"] || intent != status["intent_id"] {
						items[i] = map[string]any{"ref": checkpointRef, "status": "refresh_required", "navigation_only": true}
						changed = true
					}
				}
				if changed {
					if !cloned {
						req = cloneToolMessages(req)
						cloned = true
					}
					raw, _ := json.Marshal(value)
					b := &req.Messages[mi].Content[bi]
					b.Content = append([]llm.ContentBlock(nil), b.Content...)
					b.Content[ci].Text = string(raw)
				}
			}
		}
	}
	if status["status"] == "absent" {
		return req, nil
	}
	if body != nil && status["status"] == "current" {
		// Whole fields/claims only; no truncated JSON or fabricated empty results.
		status["stage"], status["goal"] = body.Stage, body.Goal
		remaining := 1400
		for _, group := range []struct {
			name string
			rows []checkpointClaim
		}{{"unresolved", body.Unresolved}, {"next_steps", body.NextSteps}, {"prerequisites", body.Prerequisites}, {"verified", body.Verified}, {"unverified", body.Unverified}, {"hypotheses", body.Hypotheses}} {
			selected := []checkpointClaim{}
			for _, row := range group.rows {
				raw, _ := json.Marshal(row)
				if n := utf8.RuneCount(raw); n <= remaining {
					selected = append(selected, row)
					remaining -= n
				}
			}
			status[group.name] = map[string]any{"items": selected, "has_more": len(selected) < len(group.rows), "read": map[string]any{"items": []contextRead{{Ref: checkpointRef, Field: "/" + group.name}}}}
		}
	}
	raw, err := boundedCheckpointStatus(status)
	if err != nil {
		return req, err
	}
	req.System = append(req.System, checkpointPrefix+string(raw)+"\n以上为Agent保存的进度，不是事实/授权/目标达成证明。阶段变化或暂停前可用TodoWrite的checkpoint保存，不要复制报文；关键判断回读原始证据，冲突/未验证内容不能当已证实。")
	return req, nil
}

func boundedCheckpointStatus(status map[string]any) ([]byte, error) {
	for {
		raw, err := json.Marshal(status)
		if err != nil || utf8.RuneCount(raw) <= 3500 {
			return raw, err
		}
		shrunk := false
		for _, name := range []string{"hypotheses", "unverified", "verified", "prerequisites", "next_steps", "unresolved"} {
			group, _ := status[name].(map[string]any)
			items, _ := group["items"].([]checkpointClaim)
			if len(items) > 0 {
				group["items"], group["has_more"] = items[:len(items)-1], true
				shrunk = true
				break
			}
		}
		if shrunk {
			continue
		}
		if _, ok := status["goal"]; ok {
			delete(status, "goal")
			status["goal_omitted"] = true
			status["goal_read"] = map[string]any{"items": []contextRead{{Ref: checkpointRef, Field: "/goal"}}}
			continue
		}
		// Recoverable preview only. Do not cut JSON, labels or a whole claim.
		return json.Marshal(map[string]any{"ref": checkpointRef, "version": status["version"], "status": status["status"], "navigation_only": true, "preview_omitted": true, "read": map[string]any{"items": []contextRead{{Ref: checkpointRef}}}})
	}
}
