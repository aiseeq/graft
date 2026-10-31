//go:build linux

package loadgate

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// linuxClockTicks is the Linux procfs user and system CPU clock rate.
const linuxClockTicks = 100

func treeCPU(root int) (float64, error) {
	procRoot := os.Getenv(ProcEnv)
	if procRoot == "" {
		procRoot = "/proc"
	}
	queue := []int{root}
	seen := map[int]bool{}
	var total float64
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read process %d CPU: %w", pid, err)
		}
		line := string(data)
		close := strings.LastIndex(line, ")")
		if close < 0 {
			return 0, fmt.Errorf("/proc/%d/stat: malformed command", pid)
		}
		f := strings.Fields(line[close+1:])
		if len(f) < 15 {
			return 0, fmt.Errorf("/proc/%d/stat: missing CPU fields", pid)
		}
		for _, i := range []int{11, 12, 13, 14} {
			n, e := strconv.ParseInt(f[i], 10, 64)
			if e != nil {
				return 0, fmt.Errorf("/proc/%d/stat CPU: %w", pid, e)
			}
			total += float64(n) / linuxClockTicks
		}
		matches, err := filepath.Glob(filepath.Join(procRoot, strconv.Itoa(pid), "task", "*", "children"))
		if err != nil {
			return 0, err
		}
		for _, path := range matches {
			b, e := os.ReadFile(path)
			if proc.Gone(e) {
				continue
			}
			if e != nil {
				return 0, e
			}
			for s := range strings.FieldsSeq(string(b)) {
				kid, e := strconv.Atoi(s)
				if e != nil {
					return 0, e
				}
				queue = append(queue, kid)
			}
		}
	}
	return total, nil
}
