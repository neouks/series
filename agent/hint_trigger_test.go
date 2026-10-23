package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHintTriggerBatchAndFallback(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("hint trigger test", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "human")
	var calls, fallback int
	var texts []string
	ts.SetNotify(func() { fallback++ })
	ts.SetNotifyHint(func(v []string) { calls++; texts = v })
	_, err = ts.addHint().Call(t.Context(), json.RawMessage(`{"hints":[{"text":" first "},{"text":"second"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || fallback != 0 || !reflect.DeepEqual(texts, []string{"first", "second"}) {
		t.Fatalf("calls=%d fallback=%d texts=%v", calls, fallback, texts)
	}
	var ids []int64
	tsxKind := ""
	ts.SetNotifyNodes(func(kind string, nodes []int64) { tsxKind = kind; ids = nodes })
	if _, err = ts.addHint().Call(t.Context(), json.RawMessage(`{"hints":[{"text":"identified first"},{"text":"identified second"}]}`), nil); err != nil {
		t.Fatal(err)
	}
	if tsxKind != "hint" || len(ids) != 2 || ids[0] == 0 || ids[1] == 0 || calls != 1 {
		t.Fatal(tsxKind, ids, calls)
	}
	rendered := ts.plannerBootstrap(t.Context(), []TriggerEvent{{Kind: "hint", NodeIDs: ids, Hints: []string{"forged"}}})
	if !strings.Contains(rendered, "identified first") || !strings.Contains(rendered, "identified second") || strings.Contains(rendered, "forged") {
		t.Fatal(rendered)
	}
	ts.SetNotifyNodes(nil)

	ts.SetNotifyHint(nil)
	_, err = ts.addHint().Call(t.Context(), json.RawMessage(`{"text":"fallback"}`), nil)
	if err != nil || fallback != 1 {
		t.Fatalf("fallback=%d err=%v", fallback, err)
	}
}
