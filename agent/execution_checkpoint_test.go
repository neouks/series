package agent

import (
	"strings"
	"testing"
)

func TestDecodeCheckpointIdentifiesInvalidClaimReference(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"missing source", `"next_steps":[{"text":"下一项","refs":["node:42"]}]`, "checkpoint.next_steps[0].refs[0] 的 node:42 缺少对应来源"},
		{"invalid type", `"verified":[{"text":"结论","refs":["activity:42"]}]`, "checkpoint.verified[0].refs[0] 需要 node:ID"},
		{"matching source", `"verified":[{"text":"结论","refs":["node:42"]}],"sources":[{"ref":"node:42","stamp":"0123456789abcdef0123456789abcdef"}]`, ""},
		{"unverified without reference", `"unverified":[{"text":"待确认"}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeCheckpoint([]byte(`{"base_version":0,"stage":"当前阶段","goal":"任务目标",` + tc.input + `}`))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
