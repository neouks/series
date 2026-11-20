//go:build liveeval

package server

import (
	"fmt"
	"strings"
	"testing"
)

func builtinPressureSteps(scenario string) []string {
	optional, gap := []string{"list_finding_deletion_feedback"}, 8
	if scenario == "pressure_mixed" {
		optional, gap = []string{"list_finding_deletion_feedback", "add_task_scope", "add_company_scope"}, 6
	}
	steps := append([]string(nil), optional...)
	for cycle := 0; cycle < 4; cycle++ {
		for read := 0; read < gap; read++ {
			steps = append(steps, "context_get")
		}
		steps = append(steps, optional...)
	}
	return append(steps, "record_fact")
}

// Unique bounded evidence bodies are delivered to the REAL provider on every
// context read. No schemas, instructions, timing or eviction state are injected.
func builtinPressureExcerpt(seed string, position int) string {
	var b strings.Builder
	for i := 0; b.Len() < 6000; i++ {
		fmt.Fprintf(&b, "观察记录%d：关联凭据指纹%s。该条记录来自隔离任务的只读取证窗口，已记录请求时间、目标归属、证据来源与复核状态。正文保留观测摘要，未执行外部操作。\n", i, liveReceipt(seed+"/evidence", position*100+i))
	}
	return string([]rune(b.String())[:1200])
}

func TestLiveBuiltinPressureFixtureBounds(t *testing.T) {
	for _, s := range []string{"pressure_revisit", "pressure_mixed"} {
		f := newBuiltinLiveFixture(t, s, 0)
		if !f.pressure || len(f.steps) < 38 {
			t.Fatal("missing long fixture")
		}
		a, b := builtinPressureExcerpt(f.seed, 0), builtinPressureExcerpt(f.seed, 1)
		if len([]rune(a)) != 1200 || a == b || strings.Contains(a, "next_instruction") {
			t.Fatal("invalid bounded evidence")
		}
		// Repeated uses must require new arguments, not replay earlier parameters.
		previous := map[string]string{}
		for i, n := range f.steps {
			instruction := f.instruction(i)
			if instruction == previous[n] {
				t.Fatal("repeated static arguments", n)
			}
			previous[n] = instruction
		}
	}
}
