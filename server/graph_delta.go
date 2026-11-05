package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/neouks/series/db"
)

// Only summary windows are cached, never node bodies. Every request completes
// the live scoped DB read first; this cache reduces transfer, not authorization.
var graphTransfers = struct {
	sync.Mutex
	pages map[string]db.GraphViewPage
	order []string
}{pages: map[string]db.GraphViewPage{}}

func writeGraphWindow(w http.ResponseWriter, r *http.Request, page db.GraphViewPage, task string) {
	w.Header().Set("Cache-Control", "no-store")
	if page.Unchanged {
		writeJSON(w, 200, struct {
			db.GraphViewPage
			TaskID string `json:"task_id,omitempty"`
		}{page, task})
		return
	}
	q := r.URL.Query()
	prior := q.Get("revision")
	q.Del("revision")
	key := r.URL.Path + "?" + q.Encode()
	graphTransfers.Lock()
	old, ok := graphTransfers.pages[key]
	if _, found := graphTransfers.pages[key]; !found {
		if len(graphTransfers.order) >= 32 {
			delete(graphTransfers.pages, graphTransfers.order[0])
			graphTransfers.order = graphTransfers.order[1:]
		}
		graphTransfers.order = append(graphTransfers.order, key)
	}
	graphTransfers.pages[key] = page
	graphTransfers.Unlock()
	out := struct {
		db.GraphViewPage
		TaskID string `json:"task_id,omitempty"`
	}{page, task}
	if ok && old.Revision == prior && prior != "" {
		delta := db.MakeGraphDelta(old, page)
		patch := out
		patch.Nodes = nil
		patch.Edges = nil
		patch.Delta = &delta
		patch.BaseRevision = prior
		a, _ := json.Marshal(patch)
		b, _ := json.Marshal(out)
		if len(a) < len(b) {
			out = patch
		}
	}
	writeJSON(w, 200, out)
}
