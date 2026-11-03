package evidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neouks/series/db"
)

func TestReproductionBindingsAreScopedStableAndAtomic(t *testing.T) {
	s, in, _ := evidenceFixture(t)
	seedExchange(t, s, "reproduction-proof", []byte("真实响应🙂"), true)
	refs := []db.TrafficRef{{TrafficID: "reproduction-proof", Role: "proof"}}
	in.Reproduction = &db.FindingReproduction{Steps: []db.FindingReproductionStep{{Title: "确认", Action: "读取已有记录", Result: "返回真实响应", TrafficRefs: refs}}}
	f, err := s.Record(t.Context(), in, refs)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.DB.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	var reproduction db.FindingReproduction
	if err = json.Unmarshal(stored.Reproduction, &reproduction); err != nil {
		t.Fatal(err)
	}
	if len(reproduction.Steps[0].BindingIDs) != 1 || reproduction.Steps[0].BindingIDs[0] != fmt.Sprint(f.Traffic.Bindings[0].ID) || len(reproduction.Steps[0].TrafficRefs) != 0 {
		t.Fatal(string(stored.Reproduction))
	}
	other, err := s.Record(t.Context(), in, refs)
	if err != nil {
		t.Fatal(err)
	}
	if other.Traffic.Bindings[0].ID == f.Traffic.Bindings[0].ID {
		t.Fatal("reused another finding's binding")
	}
	// A model cannot submit the other finding's stable binding ID.
	in.Reproduction.Steps[0].BindingIDs = []string{fmt.Sprint(other.Traffic.Bindings[0].ID)}
	if _, err = s.Record(t.Context(), in, refs); err == nil {
		t.Fatal("accepted forged binding")
	}
	in.Reproduction.Steps[0].BindingIDs = nil
	// A step must refer to a binding created by this very transaction.
	if _, err = s.Record(t.Context(), in, nil); err == nil {
		t.Fatal("unbound step accepted")
	}
	// Clearing captured traffic must not destroy the independent evidence.
	if _, err = s.Traffic.DB().Exec(`DELETE FROM exchanges`); err != nil {
		t.Fatal(err)
	}
	err = s.WithBinding(t.Context(), f.FindingID, f.Traffic.Bindings[0].ID, func(b db.FindingTrafficBinding) error {
		body, _, err := s.OpenBody(b.Snapshot, "response")
		if err != nil {
			return err
		}
		defer body.Close()
		buf := make([]byte, 100)
		n, _ := body.Read(buf)
		if !strings.Contains(string(buf[:n]), "真实响应") {
			t.Fatal("snapshot lost")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
