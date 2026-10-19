package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTaskFindingsPageScopeLegacyAndOrdering(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	create := func(sources ...int64) *Task {
		t.Helper()
		task, e := d.CreateTaskWithOptions("paged findings", "", TaskCreateOptions{SourceTaskIDs: sources})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); d.DeleteTask(task.ID) })
		return task
	}
	grand := create()
	source := create(grand.ID)
	owner := create(source.ID)
	other := create()
	for _, task := range []*Task{grand, source, owner, other} {
		if _, err = d.AddFinding(task.ID, 0, "test", "name", "high", "summary", "full evidence", "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	var legacy, persisted int64
	for _, dest := range []*int64{&legacy, &persisted} {
		if err = d.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) VALUES($1,'finding','confirmed','{"summary":"legacy","severity":"low","evidence":"旧证据"}') RETURNING id`, source.ExplorationID).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = d.AddFinding(source.ID, persisted, "test", "dedup", "high", "persisted summary", "detail", "test", nil); err != nil {
		t.Fatal(err)
	}
	// Exceed the former 200-node cap; all pages remain accessible and disjoint.
	for i := 0; i < 205; i++ {
		if _, err = d.AddFinding(owner.ID, 0, "test", fmt.Sprint(i), "low", "summary", "large body", "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	var ordered []string
	for page := 1; page <= 42; page++ {
		rows, total, e := d.ListTaskFindingsPage(t.Context(), owner.ID, page, 5, false)
		if e != nil {
			t.Fatal(e)
		}
		if total != 209 {
			t.Fatalf("total=%d, want 209", total)
		}
		for _, f := range rows {
			if f.TaskID == nil || (*f.TaskID != owner.ID && *f.TaskID != source.ID) {
				t.Fatalf("scope leak %+v", f)
			}
			key := fmt.Sprintf("%d:%v", f.ID, f.NodeID)
			if f.NodeID != nil {
				key = fmt.Sprintf("%d:%d", f.ID, *f.NodeID)
			}
			if seen[key] {
				t.Fatalf("duplicate %s", key)
			}
			seen[key] = true
			ordered = append(ordered, key)
			if f.ID == 0 && (*f.NodeID != legacy || f.Evidence != "") {
				t.Fatalf("legacy %+v", f)
			}
			if f.ID != 0 && f.Evidence != "" {
				t.Fatal("list loaded persistent evidence")
			}
		}
	}
	if len(seen) != 209 {
		t.Fatal(len(seen))
	}
	full, e := d.GetTaskLegacyFinding(t.Context(), owner.ID, legacy)
	if e != nil || full == nil || full.ID != 0 || full.Evidence != "旧证据" {
		t.Fatalf("legacy detail %+v %v", full, e)
	}
	for _, scope := range []int64{other.ID, grand.ID} {
		full, e = d.GetTaskLegacyFinding(t.Context(), scope, legacy)
		if e != nil || full != nil {
			t.Fatalf("legacy scope leak %+v %v", full, e)
		}
	}
	full, e = d.GetTaskLegacyFinding(t.Context(), owner.ID, persisted)
	if e != nil || full != nil {
		t.Fatalf("persisted treated as legacy %+v %v", full, e)
	}
	rows, total, err := d.ListTaskFindingsPage(t.Context(), owner.ID, 43, 5, false)
	if err != nil || total != 209 || len(rows) != 0 {
		t.Fatalf("empty page %d %v", total, err)
	}
	rows, _, err = d.ListTaskFindingsPage(t.Context(), owner.ID, 1, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	first := fmt.Sprintf("%d:%v", rows[0].ID, rows[0].NodeID)
	if first != ordered[len(ordered)-1] {
		t.Fatalf("reverse ordering %s %s", first, ordered[len(ordered)-1])
	}
	if _, err = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, source.ID); err != nil {
		t.Fatal(err)
	}
	_, total, err = d.ListTaskFindingsPage(t.Context(), owner.ID, 1, 20, false)
	if err != nil || total != 206 {
		t.Fatalf("deleted source included: %d %v", total, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = d.ListTaskFindingsPage(ctx, owner.ID, 1, 20, false); err == nil {
		t.Fatal("cancel swallowed")
	}
	for _, args := range [][2]int{{0, 20}, {1, 201}, {1, 0}, {1000001, 20}} {
		if _, _, err = d.ListTaskFindingsPage(t.Context(), owner.ID, args[0], args[1], false); err == nil {
			t.Fatal("invalid pagination accepted")
		}
	}
}

func TestTaskFindingsPageFiltersBeforePaging(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	create := func(sources ...int64) *Task {
		task, e := d.CreateTaskWithOptions("filter test", "", TaskCreateOptions{SourceTaskIDs: sources})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM findings WHERE task_id=$1`, task.ID); d.DeleteTask(task.ID) })
		return task
	}
	source := create()
	owner := create(source.ID)
	unrelated := create()
	for _, task := range []*Task{source, owner, unrelated} {
		for i := 0; i < 3; i++ {
			f, e := d.AddFinding(task.ID, 0, "HTTP", "中文 ADMIN %_", "high", "mixed Case", "do not preload evidence", "test", nil)
			if e != nil {
				t.Fatal(e)
			}
			if i == 0 {
				if _, e = d.Exec(`UPDATE findings SET status='confirmed' WHERE id=$1`, f); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) VALUES($1,'finding','confirmed','{"name":"中文 ADMIN %_","severity":"low","summary":"legacy","evidence":"private body"}')`, source.ExplorationID); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		filter TaskFindingFilter
		want   int
	}{
		{"literal case insensitive", TaskFindingFilter{Query: "admin %_"}, 7},
		{"scope", TaskFindingFilter{Scope: "local"}, 3},
		{"inherited with legacy", TaskFindingFilter{Scope: "inherited"}, 4},
		{"combined", TaskFindingFilter{Scope: "inherited", Severity: "high", Status: "confirmed", Query: "中文"}, 1},
		{"legacy pending", TaskFindingFilter{Scope: "inherited", Severity: "low", Status: "pending"}, 1},
		{"summary", TaskFindingFilter{Query: "cAsE"}, 6},
		{"type", TaskFindingFilter{Query: "http"}, 6},
		{"literal not SQL wildcard", TaskFindingFilter{Query: "admin%"}, 0},
		{"evidence stays lazy", TaskFindingFilter{Query: "private body"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := map[string]bool{}
			for page := 1; page <= tt.want+1; page++ {
				rows, total, e := d.ListTaskFindingsPage(t.Context(), owner.ID, page, 1, false, tt.filter)
				if e != nil || total != tt.want {
					t.Fatalf("total=%d want=%d err=%v", total, tt.want, e)
				}
				if page > tt.want && len(rows) != 0 {
					t.Fatal("extra page")
				}
				for _, f := range rows {
					if f.TaskID == nil || *f.TaskID == unrelated.ID || f.Evidence != "" {
						t.Fatalf("scope or evidence leak %+v", f)
					}
					node := int64(0)
					if f.NodeID != nil {
						node = *f.NodeID
					}
					key := fmt.Sprintf("%d:%d", f.ID, node)
					if seen[key] {
						t.Fatal("overlapping pages")
					}
					seen[key] = true
				}
			}
			if len(seen) != tt.want {
				t.Fatalf("seen %d", len(seen))
			}
		})
	}
	for _, f := range []TaskFindingFilter{{Scope: "recursive"}, {Status: "bogus"}, {Severity: "info"}, {Query: strings.Repeat("中", 201)}} {
		if _, _, err = d.ListTaskFindingsPage(t.Context(), owner.ID, 1, 20, false, f); err == nil {
			t.Fatalf("invalid filter accepted %+v", f)
		}
	}
	if err = (TaskFindingFilter{Query: strings.Repeat("中", 200)}).Validate(); err != nil {
		t.Fatal(err)
	}
}
