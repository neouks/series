package db

import (
	"fmt"
	"strings"
	"testing"
)

func TestFindingCommitAuthorizationUsesTransactionAndScopeLock(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("finding auth commit", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	id, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("transaction-%d.test", task.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	in := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, AssetIDs: []int64{id}, AccessRole: "mainagent"}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := LockFindingRecordTx(tx, in); err != nil {
		t.Fatal(err)
	}
	// This is a distinct database session: process-local locking is insufficient.
	var acquired bool
	if err := d.QueryRow(`SELECT pg_try_advisory_xact_lock($1)`, companyScopeMutationLock).Scan(&acquired); err != nil || acquired {
		t.Fatal("asset approval can race commit", acquired, err)
	}
	if err := ValidateFindingAccessTx(tx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE task_asset_links SET approval_state='revoked' WHERE task_id=$1 AND asset_id=$2`, task.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFindingAccessTx(tx, in); err == nil || !strings.Contains(err.Error(), "授权") {
		t.Fatal("validation escaped transaction snapshot", err)
	}
}

func TestFindingCommitRejectsRestrictedAncestor(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("finding lineage", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	assets := d.Assets()
	es := d.Exploration(task.ExplorationID)
	a, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("ancestor-%d.test", task.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := es.AddNode(KindFact, map[string]any{"summary": "ancestor"}, 0, "confirmed", "test", []int64{a})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := es.AddIntent(map[string]any{"summary": "current"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(parent, RelDerivedFrom, intent); err != nil {
		t.Fatal(err)
	}
	if err := assets.RevokeTaskAssets(task.ID, []int64{a}, "user", ""); err != nil {
		t.Fatal(err)
	}
	in := RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intent, AccessRole: "mainagent"}
	if _, err := es.RecordFinding(t.Context(), in); err == nil {
		t.Fatal("empty explicit asset list bypassed ancestor")
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, task.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
