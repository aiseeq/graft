//go:build !linux

package stats

func treeMemory(int) (int64, error) { return 0, ErrNoTree }
