package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// contextTaskID accepts either JSON representation without passing IDs through
// float64 (which can round large IDs into a different task). The empty string
// retains the existing optional-selector behavior.
type contextTaskID string

func (id *contextTaskID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	value := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if value == "" {
			*id = ""
			return nil
		}
	}
	invalid := func() error {
		return fmt.Errorf("task_id 必须为正整数或数字字符串，且不能超出 int64 范围")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return invalid()
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return invalid()
	}
	*id = contextTaskID(strconv.FormatInt(n, 10))
	return nil
}

func contextTaskSchema() map[string]any {
	return map[string]any{
		"description": "任务ID（正整数或数字字符串）；省略或传当前任务ID使用本任务权限，其他任务须跨任务授权",
		"anyOf": []any{
			map[string]any{"type": "string", "pattern": "^[0-9]*$"},
			map[string]any{"type": "integer", "minimum": 1},
		},
	}
}

// Only the host-bound ToolSet establishes local scope. A caller-supplied task
// parameter (or RunInfo without a bound store) cannot grant access to a task.
func (t *ToolSet) contextCrossTask(task string) bool {
	return task != "" && (t == nil || t.taskID <= 0 || task != strconv.FormatInt(t.taskID, 10))
}
