//go:build windows

package app

import (
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

func availableFreeSpace(path string) (int64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("convert filesystem path: %w", err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return 0, fmt.Errorf("get filesystem free space: %w", err)
	}
	if available > math.MaxInt64 {
		return 0, fmt.Errorf("available space is too large to represent")
	}
	return int64(available), nil
}
