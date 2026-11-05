package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/memory"
	actool "github.com/Autumn-27/norma/tool"
)

// Worker memory is explicitly previewed by the host. Keep SDK write and query
// behavior, and add exact, paged recall for the catalog's file references (also
// usable for Chinese names that the SDK's ASCII query tokenizer cannot find).
// Other callers that request SDK automatic injection/index remain unchanged.
func configureWorkerMemory(opts *agentcore.Options) {
	if opts.Memory == nil || opts.Memory.Store == nil || opts.Memory.AutoInject || opts.Memory.IncludeIndexInPrompt {
		return
	}
	store := opts.Memory.Store
	opts.Tools = append(opts.Tools, &catalogRecallTool{CoreTool: memory.RecallTool(store), store: store}, memory.RecordTool(store))
	opts.Memory = nil // The same tools must not be injected again by the SDK.
}

type catalogRecallTool struct {
	actool.CoreTool
	store      *memory.Store
	schemaOnce sync.Once
	schema     map[string]any
}

func (t *catalogRecallTool) Description() string {
	return "读取历史经验（非当前证据）：query关键词搜索；name精确读取，按next续页。"
}

func (t *catalogRecallTool) InputSchema() map[string]any {
	t.schemaOnce.Do(func() {
		t.schema = obj(map[string]any{
			"query":     map[string]any{"type": "string", "description": "关键词搜索，与name二选一"},
			"name":      map[string]any{"type": "string", "description": "预览中的确切.md文件名"},
			"offset":    map[string]any{"type": "integer", "minimum": 0, "description": "name续页UTF-8字节位置，沿用next，默认0"},
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 16000, "description": "name读取Unicode字符数，默认4000"},
			"version":   map[string]any{"type": "string", "description": "沿用next的版本，变化后重读"},
		})
	})
	return t.schema
}

func (t *catalogRecallTool) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if err := actool.ValidateInput(t.InputSchema(), raw); err != nil {
		return actool.Errorf("记忆读取参数错误: " + err.Error()), nil
	}
	var in struct {
		Query    string `json:"query"`
		Name     string `json:"name"`
		Offset   int64  `json:"offset"`
		MaxChars *int   `json:"max_chars"`
		Version  string `json:"version"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return actool.Errorf("记忆读取参数错误: " + err.Error()), nil
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return actool.Errorf("记忆参数必须是单个JSON对象"), nil
	}
	if in.Name == "" {
		if strings.TrimSpace(in.Query) == "" || in.Offset != 0 || in.MaxChars != nil || in.Version != "" {
			return actool.Errorf("请提供query，或使用name精确读取及其续页参数"), nil
		}
		if err := ctx.Err(); err != nil {
			return actool.Result{}, err
		}
		return t.CoreTool.Call(ctx, raw, tc)
	}
	if strings.TrimSpace(in.Query) != "" || (in.Offset > 0 && in.Version == "") {
		return actool.Errorf("name精确读取不与query混用；续读请使用next返回的offset和version"), nil
	}
	limit := 4000
	if in.MaxChars != nil {
		limit = *in.MaxChars
	}
	if limit < 1 || limit > 16000 || in.Offset < 0 || len(in.Name) > 255 {
		return actool.Errorf("记忆参数超出范围"), nil
	}
	if err := ctx.Err(); err != nil {
		return actool.Result{}, err
	}
	root, err := filepath.Abs(t.store.Dir)
	if err != nil {
		return actool.Errorf("记忆目录不可用"), nil
	}
	entry, _, err := memoryMetadata(root, in.Name)
	if err != nil {
		return actool.Errorf("记忆不可读取: " + err.Error()), nil
	}
	if in.Version != "" && in.Version != entry.version {
		return actool.Errorf("记忆版本已变化，请从offset=0重新读取"), nil
	}
	f, info, err := openMemoryFile(root, in.Name)
	if err != nil {
		return actool.Errorf("记忆不可读取: " + err.Error()), nil
	}
	defer f.Close()
	if memoryFileVersion(info) != entry.version || in.Offset > info.Size()-entry.body {
		return actool.Errorf("记忆版本或offset无效，请从offset=0重新读取"), nil
	}
	if _, err := f.Seek(entry.body+in.Offset, io.SeekStart); err != nil {
		return actool.Errorf("记忆定位失败"), nil
	}
	if tc != nil && tc.MaxOutputChars > 0 {
		ctx = context.WithValue(ctx, readBudgetKey{}, min(readOutputBudget(ctx), tc.MaxOutputChars))
	}
	limit = readContentBudget(ctx, limit, 1)
	text, consumed, more, err := readMemoryRunes(ctx, f, limit)
	if err != nil {
		return actool.Errorf("记忆读取失败: " + err.Error()), nil
	}
	after, err := f.Stat()
	if err != nil || memoryFileVersion(after) != entry.version {
		return actool.Errorf("记忆读取期间变化，请重试"), nil
	}
	result := map[string]any{"name": entry.name, "file": in.Name, "version": entry.version, "offset": in.Offset, "content": text, "has_more": more, "evidence": "历史经验，非当前目标事实"}
	if more {
		result["next"] = map[string]any{"name": in.Name, "version": entry.version, "offset": in.Offset + consumed, "max_chars": limit}
	}
	return contextJSON(ctx, result)
}

func memoryPreviewRead(file, version string) string {
	raw, _ := json.Marshal(map[string]any{"name": file, "version": version})
	return fmt.Sprintf("RecallMemory %s", raw)
}
