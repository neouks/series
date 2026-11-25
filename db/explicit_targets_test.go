package db

import (
	"encoding/json"
	"testing"
)

func TestParseTargetEndpoint(t *testing.T) {
	for _, tc := range []struct {
		input string
		host  string
		port  int
	}{
		{"example.com:8080", "example.com", 8080},
		{"1.1.1.1", "1.1.1.1", 80},
		{"1.1.1.1:9090", "1.1.1.1", 9090},
		{"example.com", "example.com", 443},
		{"http://example.com/path", "example.com", 80},
	} {
		target, ok := ParseTargetEndpoint(tc.input)
		if !ok || target != (TargetEndpoint{Host: tc.host, Port: tc.port}) {
			t.Errorf("%q: %+v, %v", tc.input, target, ok)
		}
	}
	for _, raw := range []string{"example.com:99999", "*.example.com", "1.1.1.1:0", "not a target"} {
		if target, ok := ParseTargetEndpoint(raw); ok {
			t.Errorf("%q unexpectedly parsed as %+v", raw, target)
		}
	}
}

func TestExplicitTargetsRestrictPortsAndDiscovery(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	description := "测试 explicit-port.test:8080、192.0.2.44 和 192.0.2.45:9090"
	task, err := d.CreateTaskWithOptions(description, "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	as := d.Assets()
	for _, target := range []struct{ host, evidence string }{
		{"explicit-port.test", "explicit-port.test:8080"},
		{"192.0.2.44", "192.0.2.44"},
		{"192.0.2.45", "192.0.2.45:9090"},
	} {
		if _, err := as.RegisterDescriptionAsset(task.ID, "host", target.host, target.evidence); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		host string
		port int
		want bool
	}{
		{"explicit-port.test", 8080, true},
		{"explicit-port.test", 80, false},
		{"explicit-port.test", 443, false},
		{"sub.explicit-port.test", 8080, false},
		{"192.0.2.44", 80, true},
		{"192.0.2.44", 443, false},
		{"192.0.2.45", 9090, true},
		{"192.0.2.45", 80, false},
	} {
		if err := as.ValidateExplicitTarget(task.ID, tc.host, tc.port); (err == nil) != tc.want {
			t.Errorf("%s:%d allowed=%v want=%v (%v)", tc.host, tc.port, err == nil, tc.want, err)
		}
	}
	for _, summary := range []string{"扫描 explicit-port.test 的其他端口", "端口扫描", "子域名探测", "nmap -p- explicit-port.test", "测试 sub.explicit-port.test:8080", "测试 192.0.2.45:80", "测试 192.0.2.44 的 9090 端口"} {
		if err := as.ValidateExplicitIntent(task.ID, summary); err == nil {
			t.Errorf("intent allowed: %q", summary)
		}
	}
	if err := as.ValidateExplicitIntent(task.ID, "验证 192.0.2.45:9090 的接口"); err != nil {
		t.Fatal(err)
	}
	if err := as.ValidateExplicitIntent(task.ID, "验证 192.0.2.45 的 9090 端口"); err != nil {
		t.Fatal(err)
	}
	service, err := as.UpsertOtherService(UpsertOtherServiceReq{Domain: "explicit-port.test", Port: 8081, ServiceName: "other", TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := as.ValidateExplicitAssets(task.ID, []int64{service}); err == nil {
		t.Fatal("intent anchored to unprovided service port")
	}
	bad, _ := json.Marshal(map[string]string{"summary": "子域名枚举"})
	if err := as.ValidateIntentHosts(task.ID, &Node{Payload: bad}, nil); err == nil {
		t.Fatal("worker admission accepted discovery intent")
	}
}
