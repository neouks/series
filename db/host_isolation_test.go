package db

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Authorization is per host: blocking, revoking or deleting a domain never
// changes a different-host subdomain, and approving a domain never approves
// one. Same-host assets (DNS siblings, services, endpoints) still inherit.
func TestPerHostAuthorizationDoesNotCascadeToSubdomains(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	suffix := time.Now().UnixNano()
	rootDomain := fmt.Sprintf("iso-%d.test", suffix)
	task, err := d.CreateTaskWithOptions("per-host isolation", "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	assets := d.Assets()

	rootID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: rootDomain, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	subHost := "www." + rootDomain
	subID, err := assets.UpsertSubdomain(UpsertSubdomainReq{Domain: subHost, TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	defer assets.DeleteByIDs([]int64{rootID, subID})
	if err := assets.ApproveTaskAssets(task.ID, []int64{subID}, "user", "approve subdomain"); err != nil {
		t.Fatal(err)
	}
	stateOf := func(id int64) string {
		t.Helper()
		states, err := assets.TaskAssetApprovalStates(task.ID, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		return states[id]
	}

	t.Run("block", func(t *testing.T) {
		if err := assets.BlockTaskAssets(task.ID, []int64{rootID}, "user", "block root"); err != nil {
			t.Fatal(err)
		}
		if stateOf(subID) != ApprovalApproved {
			t.Fatalf("subdomain state after blocking root=%q, want approved", stateOf(subID))
		}
		if err := assets.ValidateTaskAssetsApproved(task.ID, []int64{subID}); err != nil {
			t.Fatalf("blocking root restricted subdomain asset: %v", err)
		}
		if err := assets.ValidateTaskHostsApproved(task.ID, []string{subHost}); err != nil {
			t.Fatalf("blocking root restricted subdomain host: %v", err)
		}
		if err := assets.ValidateTaskHostsApproved(task.ID, []string{rootDomain}); !errors.Is(err, ErrTaskAssetBlocked) {
			t.Fatalf("blocked root host validation=%v, want ErrTaskAssetBlocked", err)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		if err := assets.ApproveTaskAssets(task.ID, []int64{rootID}, "user", "restore"); err != nil {
			t.Fatal(err)
		}
		if err := assets.RevokeTaskAssets(task.ID, []int64{rootID}, "user", ""); err != nil {
			t.Fatal(err)
		}
		if stateOf(subID) != ApprovalApproved {
			t.Fatalf("subdomain state after revoking root=%q, want approved", stateOf(subID))
		}
		if err := assets.ValidateTaskHostsApproved(task.ID, []string{subHost}); err != nil {
			t.Fatalf("revoking root restricted subdomain host: %v", err)
		}
	})

	t.Run("detach", func(t *testing.T) {
		detached, err := assets.DetachAssetFromTask(task.ID, rootID)
		if err != nil || !detached {
			t.Fatalf("detach=%v err=%v", detached, err)
		}
		if stateOf(subID) != ApprovalApproved {
			t.Fatalf("subdomain state after deleting root=%q, want approved", stateOf(subID))
		}
		if err := assets.ValidateTaskHostsApproved(task.ID, []string{subHost}); err != nil {
			t.Fatalf("deleting root restricted subdomain host: %v", err)
		}
		// Deletion is per host too: a sibling subdomain stays discoverable.
		apiID, err := assets.UpsertSubdomain(UpsertSubdomainReq{Domain: "api." + rootDomain, TaskID: task.ID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		defer assets.DeleteByIDs([]int64{apiID})
		state, err := assets.RegisterAgentDiscoveredAsset(task.ID, apiID, "worker")
		if err != nil || state != ApprovalPending {
			t.Fatalf("sibling rediscovery state=%q err=%v, want pending", state, err)
		}
	})

	t.Run("approve does not cascade", func(t *testing.T) {
		if _, err := assets.AttachAssetsToTask(task.ID, []int64{rootID}, "re-add root"); err != nil {
			t.Fatal(err)
		}
		deepHost := "deep." + rootDomain
		deepID, err := assets.UpsertSubdomain(UpsertSubdomainReq{Domain: deepHost, TaskID: task.ID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		defer assets.DeleteByIDs([]int64{deepID})
		if state := stateOf(deepID); state != ApprovalPending {
			t.Fatalf("new subdomain state with approved root=%q, want pending", state)
		}
		// Planner admission still rejects a pending host; worker execution allows it.
		if err := assets.ValidateTaskHostsApproved(task.ID, []string{deepHost}); !errors.Is(err, ErrTaskAssetNotApproved) {
			t.Fatalf("pending subdomain planner validation=%v, want ErrTaskAssetNotApproved", err)
		}
		if err := assets.ValidateWorkerHosts(task.ID, []string{deepHost}); err != nil {
			t.Fatalf("pending subdomain worker validation=%v", err)
		}
	})

	t.Run("service inherits exact host only", func(t *testing.T) {
		svcID, err := assets.UpsertHTTPService(UpsertHTTPServiceReq{URL: "https://svc." + rootDomain, TaskID: task.ID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		defer assets.DeleteByIDs([]int64{svcID})
		// The approved root domain does not authorize a service on a subdomain
		// host that has no domain asset of its own.
		if state := stateOf(svcID); state != ApprovalPending {
			t.Fatalf("service state under approved root=%q, want pending", state)
		}
		if err := assets.ValidateWorkerAssets(task.ID, []int64{svcID}); err != nil {
			t.Fatalf("pending service worker validation=%v", err)
		}
		// A service on the root's own host still inherits its approval.
		rootSvcID, err := assets.UpsertHTTPService(UpsertHTTPServiceReq{URL: "https://" + rootDomain + "/", TaskID: task.ID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		defer assets.DeleteByIDs([]int64{rootSvcID})
		if state := stateOf(rootSvcID); state != ApprovalApproved {
			t.Fatalf("same-host service state=%q, want approved", state)
		}
	})

	t.Run("worker cancellation stays on the host", func(t *testing.T) {
		es := d.Exploration(task.ExplorationID)
		rootSvcID, err := assets.UpsertHTTPService(UpsertHTTPServiceReq{URL: "https://" + rootDomain + "/", TaskID: task.ID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		defer assets.DeleteByIDs([]int64{rootSvcID})
		rootSvcIntent, err := es.AddIntent(map[string]any{"summary": "root host work"}, 5, []int64{rootSvcID}, "planner")
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := es.ClaimIntent(rootSvcIntent, "worker"); err != nil || !ok {
			t.Fatalf("same-host service claim=%v err=%v", ok, err)
		}
		subIntent, err := es.AddIntent(map[string]any{"summary": "subdomain work"}, 5, []int64{subID}, "planner")
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := es.ClaimIntent(subIntent, "worker"); err != nil || !ok {
			t.Fatalf("subdomain claim=%v err=%v", ok, err)
		}
		if err := assets.RememberWorkerAccess(task.ID, subIntent, []string{subHost}, nil); err != nil {
			t.Fatal(err)
		}
		if err := assets.BlockTaskAssets(task.ID, []int64{rootID}, "user", "block root"); err != nil {
			t.Fatal(err)
		}
		running, err := assets.RunningIntentIDsForAssets(task.ID, []int64{rootID})
		if err != nil {
			t.Fatal(err)
		}
		hasRoot, hasSub := false, false
		for _, id := range running {
			if id == rootSvcIntent {
				hasRoot = true
			}
			if id == subIntent {
				hasSub = true
			}
		}
		if !hasRoot {
			t.Fatalf("same-host worker not selected: %v", running)
		}
		if hasSub {
			t.Fatalf("subdomain worker selected by root block: %v", running)
		}
	})
}

// A source task's host decision follows its assets into inheriting tasks, but
// never crosses to a different host.
func TestPerHostIsolationAcrossSourceTasks(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	suffix := time.Now().UnixNano()
	rootDomain := fmt.Sprintf("iso-src-%d.test", suffix)
	source, err := d.CreateTaskWithOptions("isolation source", "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(source.ID)
	assets := d.Assets()
	rootID, err := assets.UpsertRootDomain(UpsertRootDomainReq{Domain: rootDomain, TaskID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	subHost := "www." + rootDomain
	subID, err := assets.UpsertSubdomain(UpsertSubdomainReq{Domain: subHost, TaskID: source.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	defer assets.DeleteByIDs([]int64{rootID, subID})
	if err := assets.ApproveTaskAssets(source.ID, []int64{subID}, "user", "approve subdomain"); err != nil {
		t.Fatal(err)
	}

	task, err := d.CreateTaskWithOptions("isolation inheritor", "goal", TaskCreateOptions{
		AssetApprovalTemplate: "explicit_targets", SourceTaskIDs: []int64{source.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	if err := assets.BlockTaskAssets(source.ID, []int64{rootID}, "user", "block root in source"); err != nil {
		t.Fatal(err)
	}
	states, err := assets.TaskAssetApprovalStates(task.ID, []int64{subID})
	if err != nil {
		t.Fatal(err)
	}
	if states[subID] != ApprovalApproved {
		t.Fatalf("inherited subdomain state after source block=%q, want approved", states[subID])
	}
	if err := assets.ValidateTaskHostsApproved(task.ID, []string{subHost}); err != nil {
		t.Fatalf("source block restricted inheriting task's subdomain host: %v", err)
	}
	hostStates, err := assets.TaskHostApprovalStates(task.ID, []string{rootDomain})
	if err != nil {
		t.Fatal(err)
	}
	if hostStates[rootDomain] != ApprovalBlocked {
		t.Fatalf("inheriting task root host state=%q, want blocked", hostStates[rootDomain])
	}
}
