package traffic

import (
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/neouks/series/db"
	"github.com/neouks/series/guard"
	"github.com/neouks/series/llmrec"
)

func TestWorkerProxyPendingAndManualLimits(t *testing.T) {
	dsn := os.Getenv("SERIES_PG_DSN")
	if dsn == "" {
		t.Skip("requires explicit isolated SERIES_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("worker proxy", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	intent, err := es.AddNode(db.KindIntent, map[string]any{"summary": "proxy work"}, 1, "running", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &Traffic{assets: d.Assets()}
	request := func(scope string) bool {
		t.Helper()
		user, pass, ok := guard.TaskProxyCredentials(task.ID, scope)
		if !ok {
			t.Fatal("proxy signing unavailable")
		}
		req := httptest.NewRequest("GET", "https://pending.proxy.test/x", nil)
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		allowed, err := tr.authorizeTaskRequest(httptest.NewRecorder(), req)
		if allowed && err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Proxy-Authorization") != "" {
			t.Fatal("proxy tag leaked")
		}
		return allowed
	}
	scope := fmt.Sprintf("worker:%d", intent)
	if !request("mainagent") {
		t.Fatal("main pending blocked")
	}
	if !request(scope) {
		t.Fatal("worker pending blocked")
	}
	for _, scope := range []string{"planner", "", "worker:invalid", fmt.Sprintf("worker:%d", intent+99999)} {
		if request(scope) {
			t.Fatalf("untrusted/inactive scope allowed: %s", scope)
		}
	}
	id, err := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "pending.proxy.test", TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Assets().BlockTaskAssets(task.ID, []int64{id}, "user", ""); err != nil {
		t.Fatal(err)
	}
	if request("mainagent") {
		t.Fatal("main bypassed block")
	}
	if request(scope) {
		t.Fatal("worker bypassed block")
	}
	if err := d.Assets().ApproveTaskAssets(task.ID, []int64{id}, "user", ""); err != nil {
		t.Fatal(err)
	}
	if !request(scope) {
		t.Fatal("approval did not restore")
	}
	if err := es.SetNodeState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	if request(scope) {
		t.Fatal("completed worker token usable")
	}
}

func TestTrafficReadRolesRetainApprovalPolicy(t *testing.T) {
	dsn := os.Getenv("SERIES_PG_DSN")
	if dsn == "" {
		t.Skip("requires explicit isolated SERIES_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("traffic read roles", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	tr := &Traffic{assets: d.Assets()}
	for _, role := range []string{"planner", "reporter", "auto", "custom-reader", "mainagent", "worker", ""} {
		t.Run(role, func(t *testing.T) {
			host := fmt.Sprintf("traffic-reader-%s-%d.test", role, task.ID)
			id, err := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: host, TaskID: task.ID, AgentDiscovered: true})
			if err != nil {
				t.Fatal(err)
			}
			ri := llmrec.RunInfo{TaskID: task.ID, AgentKey: role}
			if role == "worker" {
				ri.IntentID = 1
			}
			ctx := llmrec.WithRunInfo(t.Context(), ri)
			allowedPending := role == "mainagent" || role == "worker"
			if allowed := tr.allowToolHost(ctx, host) == nil; allowed != allowedPending {
				t.Fatalf("pending allowed=%v want=%v", allowed, allowedPending)
			}
			if err := d.Assets().ApproveTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
				t.Fatal(err)
			}
			if err := tr.allowToolHost(ctx, host); err != nil {
				t.Fatalf("bound role cannot read approved host: %v", err)
			}
			if err := d.Assets().RevokeTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
				t.Fatal(err)
			}
			if tr.allowToolHost(ctx, host) == nil {
				t.Fatal("revoked host readable")
			}
		})
	}
}

// Blocking a domain is per host: the proxy stops egress to that exact host but
// a subdomain host keeps its own state (pending stays worker-executable).
func TestProxyBlockIsPerHost(t *testing.T) {
	dsn := os.Getenv("SERIES_PG_DSN")
	if dsn == "" {
		t.Skip("requires explicit isolated SERIES_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("proxy per host", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	intent, err := es.AddNode(db.KindIntent, map[string]any{"summary": "proxy per host work"}, 1, "running", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &Traffic{assets: d.Assets()}
	request := func(scope, host string) bool {
		t.Helper()
		user, pass, ok := guard.TaskProxyCredentials(task.ID, scope)
		if !ok {
			t.Fatal("proxy signing unavailable")
		}
		req := httptest.NewRequest("GET", "https://"+host+"/x", nil)
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		allowed, err := tr.authorizeTaskRequest(httptest.NewRecorder(), req)
		if allowed && err != nil {
			t.Fatal(err)
		}
		return allowed
	}
	root := "perhost.proxy.test"
	id, err := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: root, TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Assets().BlockTaskAssets(task.ID, []int64{id}, "user", ""); err != nil {
		t.Fatal(err)
	}
	if request("mainagent", root) || request(fmt.Sprintf("worker:%d", intent), root) {
		t.Fatal("blocked root host allowed")
	}
	if !request("mainagent", "sub."+root) {
		t.Fatal("root block cascaded to subdomain host")
	}
	if !request(fmt.Sprintf("worker:%d", intent), "sub."+root) {
		t.Fatal("root block cascaded to subdomain host for worker")
	}
	if request("planner", "sub."+root) {
		t.Fatal("subdomain host is approved for planner without its own approval")
	}
}
