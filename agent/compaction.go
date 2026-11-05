package agent

// The production compactor uses durable incremental metadata and bounded jobs.
// Pure topology algorithms remain in coldgraph.go; old full-payload reference
// helpers are test-only, never part of a Planner wake-up or backend binary.
import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/neouks/series/db"
	"github.com/neouks/series/llmrec"
)

type Compactor struct {
	prov     llm.Provider
	model    string
	assets   *db.AssetStore
	cooldown time.Duration
	mu       sync.Mutex
	running  map[int64]bool
}

func (c *Compactor) SetAssetStore(assets *db.AssetStore) { c.assets = assets }
func NewCompactor(prov llm.Provider, model string) *Compactor {
	return &Compactor{
		prov:     WithModelRequestScope(prov),
		model:    model,
		cooldown: 60 * time.Second,
		running:  map[int64]bool{},
	}
}

// OnPlannerRound performs only the O(1) compatibility round stamp. All graph
// scanning and summarization is queued off the planning hot path.

func (c *Compactor) OnPlannerRound(ctx context.Context, ts *db.ExplorationStore) {
	if c == nil || c.prov == nil || ts == nil {
		return
	}
	if _, err := ts.BumpRound(); err != nil {
		log.Printf("[compaction] round exp=%d: %v", ts.ID(), err)
		return
	}
	c.Schedule(ctx, ts)
}

// Graph compaction is a separate run, not a planner retry or worker intent.
// Keep other context values, but replace inherited attribution as one unit.

func compactorRunContext(ctx context.Context, ts *db.ExplorationStore) (context.Context, error) {
	taskID, err := ts.TaskIDContext(ctx)
	if err != nil || taskID <= 0 {
		if err == nil {
			err = fmt.Errorf("exploration has no owning task")
		}
		return nil, err
	}
	sessionID := fmt.Sprintf("exp%d-compactor", ts.ID())
	ctx = llmrec.WithIndependentRunInfo(ctx, RunInfo{
		TaskID: taskID, ExplorationID: ts.ID(), SessionID: sessionID,
		AgentKey: "compactor", Trigger: "graph_compaction", Phase: "graph_summary",
	})
	return transcript.WithSessionID(ctx, sessionID), nil
}

func (c *Compactor) tryStart(expID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running[expID] {
		return false
	}
	c.running[expID] = true
	return true
}

func (c *Compactor) finish(expID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, expID)
}

// --- helpers: db ↔ coldgraph ---

func nodeSummary(n *db.Node) string {
	if n == nil {
		return ""
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok {
			return s
		}
		if t, ok := p["text"].(string); ok {
			return t
		}
	}
	return ""
}
