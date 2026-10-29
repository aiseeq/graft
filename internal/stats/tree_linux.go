package stats

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/proc"
)

// readTree reads pid and its descendants now. Their memory is the sum of
// their anonymous and shared-memory resident pages (RssAnon and RssShmem of
// /proc/<pid>/status). File-backed pages, binaries and libraries most of
// all, are left out: they are shared between the processes and the kernel
// takes them back under pressure. With withIO, every process is listed with
// its writes (write_bytes of /proc/<pid>/io), unread where graft may not
// read them (sudo). A process gone meanwhile is left out.
func readTree(pid int, withIO bool) (treeNow, error) {
	now := treeNow{}
	if withIO {
		now.procs = map[int]treeProc{}
	}
	type queued struct{ pid, parent int }
	queue := []queued{{pid: pid}}
	for len(queue) > 0 {
		q := queue[0]
		p := q.pid
		queue = queue[1:]
		mem, err := processMemory(p)
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			return treeNow{}, err
		}
		now.mem += mem
		if withIO && now.ioErr == nil {
			w, err := processWrites(p)
			switch {
			case proc.Gone(err):
			case errors.Is(err, fs.ErrPermission):
				now.procs[p] = treeProc{parent: q.parent}
			case err != nil:
				now.ioErr = err
			default:
				now.procs[p] = treeProc{parent: q.parent, writes: w, readable: true}
			}
		}
		kids, err := proc.Children(p)
		if proc.Gone(err) {
			continue
		}
		if err != nil {
			return treeNow{}, err
		}
		for _, kid := range kids {
			queue = append(queue, queued{kid, p})
		}
	}
	return now, nil
}

// processWrites is what the process has caused to be written to the storage
// so far, in bytes.
func processWrites(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return 0, fmt.Errorf("process %d writes: %w", pid, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "write_bytes: "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("/proc/%d/io: write_bytes: %w", pid, err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("/proc/%d/io: no write_bytes", pid)
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
