//go:build windows

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

func startOwnedPTY(*exec.Cmd, int, int) (*os.File, error) {
	return nil, fmt.Errorf("interactive PTY is unavailable on Windows")
}
func killPTYProcess(*exec.Cmd, *os.File) {}
func ownedPTYAvailable() bool            { return false }
func readOwnedPTY(context.Context, *os.File, []byte) (int, error) {
	return 0, fmt.Errorf("PTY unavailable")
}
func writeOwnedPTY(context.Context, *os.File, []byte) (int, error) {
	return 0, fmt.Errorf("PTY unavailable")
}
