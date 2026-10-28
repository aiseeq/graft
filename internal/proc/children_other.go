//go:build !linux

package proc

// descendants needs /proc: elsewhere a stopped command's own children are
// not looked for.
func descendants(int) ([]int, error) { return nil, nil }
