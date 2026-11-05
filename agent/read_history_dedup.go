package agent

import (
	"crypto/sha256"
	"reflect"

	"github.com/Autumn-27/norma/llm"
)

const historicalReadInputBytes = 64 << 10
const historicalReadOmitted = `{"history_omitted":true,"read_from_original_call":true}`

type readHistoryPosition struct{ message, block int }
type readHistoryResult struct{ position readHistoryPosition }

// Fixed FIFO query directory, independent of the raw history size. Eviction
// means keeping a result, never deleting data or suppressing an actual read.
type readHistoryDirectory struct {
	entries     map[[32]byte]readHistoryResult
	keys        [][32]byte
	next, limit int
}

func (d *readHistoryDirectory) put(key [32]byte, value readHistoryResult) {
	if d.limit <= 0 {
		return
	}
	if d.entries == nil {
		d.entries = map[[32]byte]readHistoryResult{}
	}
	if _, ok := d.entries[key]; ok {
		d.entries[key] = value
		return
	}
	if len(d.keys) < d.limit {
		d.keys = append(d.keys, key)
	} else {
		delete(d.entries, d.keys[d.next])
		d.keys[d.next] = key
		d.next = (d.next + 1) % d.limit
	}
	d.entries[key] = value
}

func deduplicateReadHistory(req llm.CompletionRequest, maxQueries, maxPending int) llm.CompletionRequest {
	if maxQueries <= 0 || maxPending <= 0 {
		return req
	}
	type pendingRead struct {
		key  [32]byte
		name string
	}
	pending := map[string]pendingRead{}
	seenCalls := map[string]bool{}
	latest := readHistoryDirectory{limit: maxQueries}
	var replaced []readHistoryPosition
	for mi, message := range req.Messages {
		if message.Role == llm.RoleAssistant || message.Role == "system" {
			clear(pending)
			clear(seenCalls)
		}
		for bi, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				delete(pending, block.ID)
				if block.ID == "" || len(block.ID) > 1024 {
					continue
				}
				if seenCalls[block.ID] {
					continue
				}
				if len(seenCalls) >= maxPending {
					continue
				}
				seenCalls[block.ID] = true
				if len(block.Input) > historicalReadInputBytes {
					continue
				}
				call := assetContextCall(block)
				key := normalizedReadQuery(call.Name, call.Input)
				if key != "" {
					pending[block.ID] = pendingRead{sha256.Sum256([]byte(key)), call.Name}
				}
				continue
			}
			if block.Type != llm.BlockToolResult {
				if message.Role == llm.RoleUser {
					clear(pending)
					clear(seenCalls)
				}
				continue
			}
			pendingCall, ok := pending[block.ToolUseID]
			key := pendingCall.key
			delete(pending, block.ToolUseID)
			if !ok || block.IsError {
				continue
			}
			// Calls are bounded; raw history, audit and original inputs stay intact.
			pos := readHistoryPosition{mi, bi}
			current := readHistoryResult{position: pos}
			if old, exists := latest.entries[key]; exists && reusableReadHistoryBody(req.Messages[old.position.message].Content[old.position.block], block, pendingCall.name) {
				replaced = append(replaced, old.position)
			}
			latest.put(key, current)
		}
	}
	if len(replaced) == 0 {
		return req
	}
	req = cloneToolMessages(req)
	for _, pos := range replaced {
		result := &req.Messages[pos.message].Content[pos.block]
		// Preserve content-block positions so noa reference suffixes can be
		// restored onto every original text block after this projection.
		content := make([]llm.ContentBlock, len(result.Content))
		for i := range content {
			content[i] = llm.TextBlock(historicalReadOmitted)
		}
		result.Content = content
	}
	return req
}

// Compare only bounded, pure text bodies, including block boundaries. Different
// values/metadata are not interchangeable even when query aliases coincide.
func reusableReadHistoryBody(a, b llm.ContentBlock, name string) bool {
	if a.IsError || b.IsError || len(a.Content) == 0 || len(a.Content) != len(b.Content) || len(a.Content) > 1024 {
		return false
	}
	size := 0
	for i, c := range a.Content {
		other := b.Content[i]
		size += len(c.Text)
		if size > 256<<10 || len(c.Text) != len(other.Text) {
			return false
		}
		if !reflect.DeepEqual(c, llm.TextBlock(c.Text)) || !reflect.DeepEqual(other, llm.TextBlock(other.Text)) || c.Text != other.Text {
			return false
		}
	}
	// Only equal bodies reach the protocol check; never decode an oversized or
	// changed result just for deduplication. A partial failure stays visible.
	return !partialReadFailure(name+":", b)
}
