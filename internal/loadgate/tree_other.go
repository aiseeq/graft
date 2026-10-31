//go:build !linux

package loadgate

import "fmt"

func treeCPU(int) (float64, error) {
	return 0, fmt.Errorf("process tree CPU reservations require Linux")
}
