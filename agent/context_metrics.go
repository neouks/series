package agent

import (
	"crypto/sha256"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"github.com/neouks/series/llmrec"
)

// Count only the final authorized/projected model view. Hashes and arguments
// are transient; no query or evidence text is written to the usage ledger.
func contextReadMetrics(req llm.CompletionRequest) llmrec.ContextReadMetrics {
	var result llmrec.ContextReadMetrics
	seen := map[[32]byte]bool{}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type != llm.BlockToolUse {
				continue
			}
			// Observability must not decode an arbitrarily large deferred/write
			// argument just to discover its target. Report incomplete tracking.
			if len(block.Input) > 64<<10 {
				if retrievalTool(block.Name) || retrievalTool(db.CanonicalToolUsageKey(block.Name)) {
					result.Calls++
					result.Limited = true
				} else if block.Name == actool.ExecuteExtraToolName {
					result.Limited = true
				}
				continue
			}
			call := assetContextCall(block)
			if !retrievalTool(call.Name) && !retrievalTool(db.CanonicalToolUsageKey(call.Name)) {
				continue
			}
			result.Calls++
			key := normalizedReadQuery(call.Name, call.Input)
			if key == "" {
				result.Limited = true
				continue
			}
			hash := sha256.Sum256([]byte(key))
			if seen[hash] {
				result.Repeated++
				continue
			}
			if len(seen) >= 4096 {
				result.Limited = true
				continue
			}
			seen[hash] = true
		}
	}
	return result
}
