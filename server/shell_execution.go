package server

import (
	"os/exec"
	"runtime"

	actool "github.com/Autumn-27/norma/tool"
)

// Match the official SDK's fixed shell choice. This reports availability; it
// does not select a different interpreter or fall back when one is missing.
func defaultShellMode() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "bash"
}

func shellDetectedPayload() map[string]any {
	mode := defaultShellMode()
	path, err := exec.LookPath(mode)
	out := map[string]any{
		"ok": err == nil, "os": runtime.GOOS, "mode": mode,
		"interactive": runtime.GOOS != "windows" && !actool.InteractiveShellDisabled(),
	}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["path"] = path
	}
	return out
}
