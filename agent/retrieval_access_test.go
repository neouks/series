package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neouks/series/db"
)

func TestRetrievalWindowRefreshesAccessAndRestores(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("retrieval visibility", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	as := d.Assets()
	asset, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("retrieval-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := es.AddNode(db.KindFact, map[string]any{"summary": "protected", "detail": strings.Repeat("protected-evidence", 2000)}, 0, "confirmed", "test", []int64{asset})
	if err != nil {
		t.Fatal(err)
	}
	req := retrievalFixture(12)
	// Replace fixture references with a real pending node.
	for i := range req.Messages {
		for j, b := range req.Messages[i].Content {
			b.Input = json.RawMessage(strings.ReplaceAll(string(b.Input), "node:1", fmt.Sprintf("node:%d", node)))
			for k, c := range b.Content {
				c.Text = strings.ReplaceAll(c.Text, "node:1", fmt.Sprintf("node:%d", node))
				c.Text = strings.ReplaceAll(c.Text, `"node_id":1`, fmt.Sprintf(`"node_id":%d`, node))
				b.Content[k] = c
			}
			req.Messages[i].Content[j] = b
		}
	}
	before, _ := json.Marshal(req)
	main := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "mainagent"})
	planner := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "planner"})
	// Fresh provider instances model restart/resume: no process counters needed.
	for _, ctx := range []struct {
		name    string
		planner bool
	}{{"main", false}, {"planner", true}} {
		run := main
		if ctx.planner {
			run = planner
		}
		got, e := FilterTaskAssetRequest(run, as, task.ID, req)
		if e != nil {
			t.Fatal(e)
		}
		data, _ := json.Marshal(got)
		if ctx.planner && strings.Contains(string(data), "中证据") {
			t.Fatal("planner pending content")
		}
		if !ctx.planner && !strings.Contains(string(data), "中证据") {
			t.Fatal("main pending content missing")
		}
		again, e := FilterTaskAssetRequest(run, as, task.ID, req)
		if e != nil {
			t.Fatal(e)
		}
		a, _ := json.Marshal(again)
		if string(a) != string(data) {
			t.Fatal("resume changed projection")
		}
	}
	if err = as.RevokeTaskAssets(task.ID, []int64{asset}, "user", ""); err != nil {
		t.Fatal(err)
	}
	got, err := FilterTaskAssetRequest(main, as, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "中证据") {
		t.Fatal("revoked body retained")
	}
	for _, s := range got.System {
		if strings.HasPrefix(s, retrievalDirectoryPrefix) && strings.Contains(s, fmt.Sprintf("node:%d", node)) {
			t.Fatal("revoked index retained")
		}
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("audit changed")
	}
	// No asset backend still gets a bounded projection for task-bound test/local
	// providers, without silently creating asset authorization.
	noStore, err := FilterTaskAssetRequest(main, nil, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noStore.Messages[1].Content[0].Content[0].Text, "history_omitted") {
		t.Fatal("nil-store task unbounded")
	}
}
