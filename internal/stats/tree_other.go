//go:build !linux

package stats

func readTree(int, bool) (treeNow, error) { return treeNow{}, ErrNoTree }
