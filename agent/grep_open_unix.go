//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// O_NONBLOCK prevents a FIFO replacement from hanging before we can inspect
// its descriptor. Regular-file reads retain normal blocking semantics.
func openGrepRegular(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
