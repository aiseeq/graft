package stats

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		if gone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		sum += mem
		kids, err := children(p)
		if gone(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		queue = append(queue, kids...)
	}
	return sum, nil
}

// gone tells whether err means the process exited while it was read.
func gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
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

// children are the processes every thread of pid started
// (/proc/<pid>/task/<tid>/children).
func children(pid int) ([]int, error) {
	tasks, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil {
		return nil, fmt.Errorf("process %d threads: %w", pid, err)
	}
	if len(tasks) == 0 {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/task", pid)); err != nil {
			return nil, fmt.Errorf("process %d: %w", pid, err)
		}
		return nil, errors.New("/proc has no task/*/children: the kernel lacks CONFIG_PROC_CHILDREN")
	}
	var kids []int
	for _, task := range tasks {
		data, err := os.ReadFile(task)
		if gone(err) {
			continue // the thread ended
		}
		if err != nil {
			return nil, fmt.Errorf("process %d children: %w", pid, err)
		}
		for f := range strings.FieldsSeq(string(data)) {
			kid, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", task, err)
			}
			kids = append(kids, kid)
		}
	}
	return kids, nil
}
