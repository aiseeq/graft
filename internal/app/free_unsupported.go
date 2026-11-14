//go:build !linux && !windows

package app

import "fmt"

func availableFreeSpace(string) (int64, error) {
	return 0, fmt.Errorf("--free-below is supported only on Linux and Windows")
}
