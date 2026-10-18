package agent

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"
)

func TestContextPromptMetrics(t *testing.T) {
	seeds := BuiltinPromptSeeds()
	seeds["reporter"] = ReporterDefaultPrompt
	keys := []string{}
	for k := range seeds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("role=%s code_prompt_bytes=%d sha256=%s", k, len(seeds[k]), fmt.Sprintf("%x", sha256.Sum256([]byte(seeds[k]))))
	}
}
