package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// DefaultToolUserAgent is the browser identity curl/wget present by default in
// agent Bash sessions. A fixed, current desktop-Chrome string reads as one
// consistent client — rotating identities looks like a scanner, not a person.
// An explicit -A/--user-agent on the command line still wins: flags override
// the startup files these entries point curl/wget at.
const DefaultToolUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// toolUAEnv holds the "CURL_HOME=…" / "WGETRC=…" entries proxyEnv appends to
// every Bash session once ConfigureToolUA has installed the startup files.
var toolUAEnv atomic.Value // []string

func currentToolUAEnv() []string {
	v, _ := toolUAEnv.Load().([]string)
	return v
}

// ConfigureToolUA gives agent-issued curl/wget commands a default browser
// User-Agent without touching any command line: it writes .curlrc/wgetrc under
// dir and records the env vars that point the tools at them. Existing files are
// never overwritten, so an operator edit survives restarts; calling with an
// empty dir clears the injection. Safe to call repeatedly.
func ConfigureToolUA(dir string) error {
	if dir == "" {
		toolUAEnv.Store([]string(nil))
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tool UA dir: %w", err)
	}
	curlrc := filepath.Join(dir, ".curlrc")
	if _, err := os.Stat(curlrc); os.IsNotExist(err) {
		if err := os.WriteFile(curlrc, []byte("user-agent = \""+DefaultToolUserAgent+"\"\n"), 0o644); err != nil {
			return fmt.Errorf("write .curlrc: %w", err)
		}
	}
	wgetrc := filepath.Join(dir, "wgetrc")
	if _, err := os.Stat(wgetrc); os.IsNotExist(err) {
		if err := os.WriteFile(wgetrc, []byte("user_agent = "+DefaultToolUserAgent+"\n"), 0o644); err != nil {
			return fmt.Errorf("write wgetrc: %w", err)
		}
	}
	toolUAEnv.Store([]string{"CURL_HOME=" + dir, "WGETRC=" + wgetrc})
	return nil
}
