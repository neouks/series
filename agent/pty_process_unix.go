//go:build !windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func startOwnedPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	// pty.StartWithSize sets Setsid/Setctty. Setpgid must NOT also be set.
	cmd.Cancel = func() error { killPTYProcess(cmd, nil); return nil }
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, err
	}
	raw, err := master.SyscallConn()
	if err == nil {
		var nbErr error
		err = raw.Control(func(fd uintptr) { nbErr = unix.SetNonblock(int(fd), true) })
		if err == nil {
			err = nbErr
		}
	}
	if err != nil {
		killPTYProcess(cmd, master)
		_ = master.Close()
		_ = cmd.Wait()
		return nil, err
	}
	return master, nil
}

func killPTYProcess(cmd *exec.Cmd, master *os.File) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return
	}
	if master != nil {
		// A job-control program can own a different foreground process group.
		// The terminal kernel restricts this group to its controlling session.
		if raw, err := master.SyscallConn(); err == nil {
			_ = raw.Control(func(fd uintptr) {
				if fg, e := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); e == nil && fg > 1 && fg != syscall.Getpgrp() {
					_ = syscall.Kill(-fg, syscall.SIGKILL)
				}
			})
		}
	}
	if cmd.Process.Pid != syscall.Getpgrp() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func ownedPTYAvailable() bool { return true }

// PTYs are not runtime-netpollable on every supported OS. Explicit bounded
// poll prevents EAGAIN from looking like EOF, while Close/cancel never waits on
// an uninterruptible blocking read/write. RawConn keeps descriptors alive and
// avoids File.Fd switching them back to blocking mode.
func ptyIO(ctx context.Context, f *os.File, p []byte, writing bool) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var n int
		var opErr error
		err = raw.Control(func(fd uintptr) {
			if writing {
				n, opErr = unix.Write(int(fd), p)
			} else {
				n, opErr = unix.Read(int(fd), p)
			}
		})
		if err != nil {
			return 0, err
		}
		if !errors.Is(opErr, unix.EAGAIN) && !errors.Is(opErr, unix.EINTR) {
			return n, opErr
		}
		err = raw.Control(func(fd uintptr) {
			event := int16(unix.POLLIN)
			if writing {
				event = unix.POLLOUT
			}
			_, opErr = unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: event}}, 100)
		})
		if err != nil {
			return 0, err
		}
		if opErr != nil && !errors.Is(opErr, unix.EINTR) {
			return 0, opErr
		}
	}
}

func readOwnedPTY(ctx context.Context, f *os.File, p []byte) (int, error) {
	n, err := ptyIO(ctx, f, p, false)
	if n == 0 && err == nil {
		err = io.EOF
	}
	return n, err
}
func writeOwnedPTY(ctx context.Context, f *os.File, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := ptyIO(ctx, f, p[total:], true)
		if n > 0 {
			total += n
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, fmt.Errorf("PTY write made no progress")
		}
	}
	return total, nil
}
