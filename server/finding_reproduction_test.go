package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/neouks/series/db"
)

func TestReproductionDetailScopeListAndExport(t *testing.T) {
	s, task := modeServer(t)
	tid := mustTaskID(t, task.ID)
	in := db.RecordFindingInput{TaskID: tid, Severity: "low", Summary: "source", Reproduction: &db.FindingReproduction{Steps: []db.FindingReproductionStep{{Title: "现场步骤", Action: "读取响应", Result: "记录中文结果", Response: "HTTP/1.1 200 OK\r\n\r\n中文"}}}}
	f, err := task.Store.RecordFinding(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.pg.DeleteFinding(f.FindingID) })
	other, err := s.m.CreateTask("unrelated repro", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.DeleteTask(other.ID, DeleteTaskOptions{}) })
	for _, tc := range []struct {
		task string
		code int
	}{{"", 200}, {task.ID, 200}, {other.ID, 404}} {
		r := httptest.NewRequest("GET", fmt.Sprintf("/?context_task=%s", tc.task), nil)
		r.SetPathValue("id", fmt.Sprint(f.FindingID))
		w := httptest.NewRecorder()
		s.getFinding(w, r)
		if w.Code != tc.code {
			t.Fatal(tc, w.Code, w.Body)
		}
		if w.Code == 200 {
			var dto FindingDTO
			if err = json.Unmarshal(w.Body.Bytes(), &dto); err != nil || len(dto.Reproduction) == 0 {
				t.Fatal(dto, err)
			}
		}
	}
	w := httptest.NewRecorder()
	s.findings(w, httptest.NewRequest("GET", "/?context_task="+task.ID, nil))
	var page struct {
		Items []FindingDTO `json:"items"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatal(err, w.Body)
	}
	if len(page.Items[0].Reproduction) != 0 {
		t.Fatal("list returned reproduction")
	}
	// JSON export uses the same detail DTO, and Markdown rendering has a tested
	// deterministic fallback when Reporter has not produced a report yet.
	w = httptest.NewRecorder()
	s.findingsExport(w, httptest.NewRequest("GET", fmt.Sprintf("/?scope=selected&format=json&ids=%d", f.FindingID), nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var exported []FindingDTO
	if err = json.Unmarshal(w.Body.Bytes(), &exported); err != nil || len(exported) != 1 || len(exported[0].Reproduction) == 0 {
		t.Fatal(err, w.Body)
	}
}
