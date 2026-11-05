//go:build windows

package agent

import (
	"fmt"
	"os"
)

func openGrepRegular(path string) (*os.File, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return os.Open(path)
}
