package traffic

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/neouks/series/llmrec"
)

func TestTrafficSearchDefaultPortsAndCursor(t *testing.T) {
	tr, _ := openTraffic(t)
	for i, url := range []string{"https://port.test/implicit", "https://port.test:443/explicit", "http://port.test/wrong", "http://port.test:443/cross-scheme", "https://port.test:8443/wrong"} {
		_, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path) VALUES(?,?,'port.test','GET','/',?,200,'text/plain',0,0,'')`, fmt.Sprint(i), i, url)
		if err != nil {
			t.Fatal(err)
		}
	}
	call := func(port, cursor string) map[string]any {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"host": "port.test:" + port, "limit": 1, "cursor": cursor})
		r, e := tr.searchTool().Call(t.Context(), b, nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		var out map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; ; i++ {
		if i > 5 {
			t.Fatal("paging loop")
		}
		out := call("443", cursor)
		for _, item := range out["items"].([]any) {
			id := item.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatal("duplicate", id)
			}
			seen[id] = true
		}
		if out["has_more"] == false {
			break
		}
		cursor = out["next_cursor"].(string)
	}
	if len(seen) != 3 || !seen["0"] || !seen["1"] || !seen["3"] {
		t.Fatal("default port omitted or wrong port included", seen)
	}
	out := call("80", "")
	items := out["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "2" {
		t.Fatal(out)
	}
}

func TestTrafficTaskRolesCannotFallBackToGlobalScope(t *testing.T) {
	tr, _ := openTraffic(t)
	tr.record(newFlow("scope.test", "GET", "/", nil, []byte("private body")))
	id := onlyExchangeID(t, tr)
	for _, role := range []string{"planner", "worker", "mainagent"} {
		ctx := llmrec.WithRunInfo(t.Context(), llmrec.RunInfo{AgentKey: role})
		for _, tool := range tr.Tools() {
			in := map[string]any{"host": "scope.test"}
			if tool.Name() == "traffic_get" {
				in = map[string]any{"id": id}
			}
			body, _ := json.Marshal(in)
			r, err := tool.Call(ctx, body, nil)
			if err != nil || !r.IsError {
				t.Fatal("missing task context gained global scope", role, tool.Name(), r.Flatten(), err)
			}
		}
	}
}
