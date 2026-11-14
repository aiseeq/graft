//go:build linux

package app

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func availableFreeSpace(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("stat filesystem free space: %w", err)
	}
	if stat.Bsize <= 0 || stat.Bavail > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return 0, fmt.Errorf("available space is too large to represent")
	}
	return int64(stat.Bavail * uint64(stat.Bsize)), nil
}
