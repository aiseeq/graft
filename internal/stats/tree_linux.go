package stats

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// treeMemory is the memory of pid and its descendants now: the sum of their
// anonymous and shared-memory resident pages (RssAnon and RssShmem of
// /proc/<pid>/status). File-backed pages, binaries and libraries most of
// all, are left out: they are shared between the processes and the kernel
// takes them back under pressure. A process gone meanwhile counts nothing.
func treeMemory(pid int) (int64, error) {
	var sum int64
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		mem, err := processMemory(p)
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		sum += mem
		kids, err := proc.Children(p)
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		queue = append(queue, kids...)
	}
	return sum, nil
}

func processMemory(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, fmt.Errorf("process %d memory: %w", pid, err)
	}
	var kb int64
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), ":")
		if !ok || (name != "RssAnon" && name != "RssShmem") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(value), " kB"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("/proc/%d/status: %s: %w", pid, name, err)
		}
		kb += n
	}
	return kb * 1024, nil
}
