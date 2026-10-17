package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConfigureToolUAWritesStartupFiles(t *testing.T) {
	dir := t.TempDir()
	if err := ConfigureToolUA(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureToolUA("") })

	curlrc, err := os.ReadFile(filepath.Join(dir, ".curlrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(curlrc), DefaultToolUserAgent) {
		t.Fatalf(".curlrc 缺少默认 UA: %q", curlrc)
	}
	wgetrc, err := os.ReadFile(filepath.Join(dir, "wgetrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wgetrc), DefaultToolUserAgent) {
		t.Fatalf("wgetrc 缺少默认 UA: %q", wgetrc)
	}
	// Existing files survive a re-run, so operator edits are never clobbered.
	if err := os.WriteFile(filepath.Join(dir, ".curlrc"), []byte("user-agent = \"custom\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureToolUA(dir); err != nil {
		t.Fatal(err)
	}
	curlrc, _ = os.ReadFile(filepath.Join(dir, ".curlrc"))
	if string(curlrc) != "user-agent = \"custom\"\n" {
		t.Fatalf("重复配置覆盖了用户修改: %q", curlrc)
	}
}

func TestProxyEnvCarriesUserAgentWithoutProxy(t *testing.T) {
	dir := t.TempDir()
	if err := ConfigureToolUA(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureToolUA("") })

	// Capture off (no proxy): the UA entries alone keep the Bash env non-nil —
	// this is the whole point of the startup-file approach.
	env := proxyEnv("", "")
	if env == nil {
		t.Fatal("无代理时 UA 环境未注入")
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"CURL_HOME=" + dir, "WGETRC=" + filepath.Join(dir, "wgetrc")} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env 缺少 %q: %v", want, env)
		}
	}
	if strings.Contains(joined, "HTTP_PROXY") {
		t.Fatalf("无代理时不应有代理变量: %v", env)
	}

	// Capture on: UA entries ride along with the proxy + CA block.
	full := proxyEnv("http://127.0.0.1:8788", "/tmp/ca.pem")
	joined = strings.Join(full, "\n")
	for _, want := range []string{"CURL_HOME=", "WGETRC=", "HTTP_PROXY=http://127.0.0.1:8788", "CURL_CA_BUNDLE=/tmp/ca.pem"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("完整 env 缺少 %q: %v", want, full)
		}
	}

	// Cleared config and no proxy → nil again (unchanged legacy behavior).
	_ = ConfigureToolUA("")
	if env := proxyEnv("", ""); env != nil {
		t.Fatalf("清空配置后应返回 nil: %v", env)
	}
}

// TestRealCurlPicksUpUserAgent proves the mechanism end to end: a real curl
// process, given only CURL_HOME, sends the browser UA; -A still overrides it.
func TestRealCurlPicksUpUserAgent(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl 不可用")
	}
	dir := t.TempDir()
	if err := ConfigureToolUA(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ConfigureToolUA("") })

	var mu sync.Mutex
	lastUA := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastUA = r.Header.Get("User-Agent")
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	run := func(extraArgs ...string) string {
		t.Helper()
		args := append([]string{"-s", "-o", "/dev/null", srv.URL}, extraArgs...)
		cmd := exec.Command(curl, args...)
		cmd.Env = append(os.Environ(), "CURL_HOME="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("curl %v: %v %s", args, err, out)
		}
		mu.Lock()
		defer mu.Unlock()
		return lastUA
	}
	if ua := run(); ua != DefaultToolUserAgent {
		t.Fatalf("curl 默认 UA=%q, want %q", ua, DefaultToolUserAgent)
	}
	if ua := run("-A", "pentest-override/1.0"); ua != "pentest-override/1.0" {
		t.Fatalf("curl -A 覆盖失效: %q", ua)
	}
}
