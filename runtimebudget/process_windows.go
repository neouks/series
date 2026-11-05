package runtimebudget

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Match the SDK's tree termination strategy on Windows. The control command
// has its own deadline and does not acquire a job slot (avoids cleanup deadlock).
func configureProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil || cmd.Process.Pid <= 1 {
			return os.ErrProcessDone
		}
		killProcessTree(cmd.Process.Pid)
		return cmd.Process.Kill()
	}
}
func cleanupProcess(cmd *exec.Cmd) {
	// Cancel kills the tree while the parent still has a PID. A naturally exited
	// parent cannot safely be re-targeted with taskkill because of PID reuse.
}
func killProcessTree(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "taskkill", "/PID", fmt.Sprint(pid), "/T", "/F").Run()
}
