package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	intentInputContract    = `单条传 {"summary":"具体探索方向"}；批量传 {"intents":[{"summary":"方向一"},{"summary":"方向二"}]}，最多 4 条。每条 summary 必须是非空字符串，批量时须填写在每个数组元素内。`
	intentInputErrorPrefix = "add_intent 参数错误："
	intentInputRetryLimit  = 3
)

var errIntentSummaryRequired = errors.New("summary 不能为空")

func intentInputError(detail string) string {
	return intentInputErrorPrefix + detail + "\n" + intentInputContract + " 修正参数后再试，无法补齐时结束本轮。"
}

type intentInputRetriesKey struct{}

type intentInputRetries struct {
	detail string
	count  int
}

func (r *intentInputRetries) observe(name, output string, failed bool) {
	detail := ""
	if name == "add_intent" && failed {
		output = strings.TrimPrefix(output, "Blocked by hook: ")
		if strings.HasPrefix(output, "{") {
			var batch struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(output), &batch) == nil {
				output = batch.Error
			}
		}
		if diagnostic, ok := strings.CutPrefix(output, intentInputErrorPrefix); ok {
			detail, _, _ = strings.Cut(diagnostic, "\n")
		}
	}
	if detail == "" {
		*r = intentInputRetries{}
		return
	}
	if r.detail != detail {
		r.detail, r.count = detail, 0
	}
	r.count++
}

func intentInputRetryError(ctx context.Context) error {
	if r, _ := ctx.Value(intentInputRetriesKey{}).(*intentInputRetries); r != nil && r.count >= intentInputRetryLimit {
		return intentInputStop(fmt.Sprintf("add_intent 连续 %d 次返回相同参数错误，已停止本轮：%s", r.count, r.detail))
	}
	return nil
}

type intentInputStop string

func (e intentInputStop) Error() string          { return string(e) }
func (intentInputStop) PreventModelReplay() bool { return true }
