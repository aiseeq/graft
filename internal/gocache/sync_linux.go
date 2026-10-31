//go:build linux

package gocache

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func syncCacheFS(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open cache directory: %w", err)
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}
