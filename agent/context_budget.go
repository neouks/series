package agent

import (
	"context"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
)

type readBudgetKey struct{}

func readOutputBudget(ctx context.Context) int {
	if limit, ok := ctx.Value(readBudgetKey{}).(int); ok && limit > 0 {
		return limit
	}
	return 30000
}

// Reserve room for identifiers, pointers and continuation metadata. A source
// character may occupy six bytes after JSON escaping; never cut serialized JSON.
func readContentBudget(ctx context.Context, requested, count int) int {
	return min(requested, max(1, (readOutputBudget(ctx)-1024*count)/6))
}
func contextJSON(ctx context.Context, v any) (actool.Result, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return actool.Errorf(e.Error()), nil
	}
	if len(b) > readOutputBudget(ctx) {
		return actool.Errorf("引用或字段路径元数据超出输出上限；请单条读取指定 field，或提高工具输出上限"), nil
	}
	return actool.Text(string(b)), nil
}
