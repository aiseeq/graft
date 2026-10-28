package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Children are the processes every thread of pid started
// (/proc/<pid>/task/<tid>/children).
func Children(pid int) ([]int, error) {
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
		if Gone(err) {
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

// Gone tells whether err means the process exited while it was read.
func Gone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// descendants are the processes pid started and the ones they started,
// those gone meanwhile left out.
func descendants(pid int) ([]int, error) {
	var all []int
	queue := []int{pid}
	for len(queue) > 0 {
		kids, err := Children(queue[0])
		queue = queue[1:]
		if Gone(err) {
			continue
		}
		if err != nil {
			return all, err
		}
		all = append(all, kids...)
		queue = append(queue, kids...)
	}
	return all, nil
}
