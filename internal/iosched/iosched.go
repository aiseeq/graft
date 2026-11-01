// Package iosched places admitted heavy work in a shared, low-weight cgroup.
package iosched

import "context"

// Enter moves the current process into the shared heavy-work slice when its
// current systemd scope can safely be moved. Other platforms do nothing.
// procRoot, when set, stands for /proc (a test's machine): without a cgroup
// file there it does nothing.
func Enter(ctx context.Context, pid int, procRoot string) (string, error) {
	return enter(ctx, pid, procRoot)
}
