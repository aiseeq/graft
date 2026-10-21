package stats

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DBSample is what a CgroupWatch saw: the CPU time the cgroup's processes
// used while it ran and the highest memory they held, page cache and tmpfs
// files included.
type DBSample struct {
	CPU time.Duration
	Mem int64
	Err error
}

// CgroupWatch follows a cgroup v2, such as the test database's container,
// while a task runs.
type CgroupWatch struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	sample   DBSample
}

// WatchCgroup starts following the cgroup in dir: its CPU counter now and
// its memory every watchInterval.
func WatchCgroup(dir string) *CgroupWatch {
	w := &CgroupWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer func() {
			if p := recover(); p != nil {
				w.sample.Err = fmt.Errorf("following the cgroup panicked: %v", p)
			}
		}()
		t := time.NewTicker(watchInterval)
		defer t.Stop()
		w.sample = followCgroup(dir, w.stop, t.C)
	}()
	return w
}

// followCgroup reads the memory at once and on every tick until stop.
func followCgroup(dir string, stop <-chan struct{}, tick <-chan time.Time) DBSample {
	start, err := cgroupCPU(dir)
	if err != nil {
		return DBSample{Err: err}
	}
	var s DBSample
	for {
		mem, err := cgroupMemory(dir)
		if err != nil {
			return DBSample{Err: err}
		}
		s.Mem = max(s.Mem, mem)
		select {
		case <-stop:
			end, err := cgroupCPU(dir)
			if err != nil {
				return DBSample{Err: err}
			}
			s.CPU = end - start
			return s
		case <-tick:
		}
	}
}

// Stop ends the watch and returns what it saw.
func (w *CgroupWatch) Stop() DBSample {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	return w.sample
}

// cgroupCPU is usage_usec of the cgroup's cpu.stat.
func cgroupCPU(dir string) (time.Duration, error) {
	path := filepath.Join(dir, "cpu.stat")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("the cgroup's CPU time: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "usage_usec "); ok {
			usec, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("%s: usage_usec %q: %w", path, v, err)
			}
			return time.Duration(usec) * time.Microsecond, nil
		}
	}
	return 0, fmt.Errorf("%s has no usage_usec", path)
}

// cgroupMemory is the cgroup's memory.current in bytes.
func cgroupMemory(dir string) (int64, error) {
	path := filepath.Join(dir, "memory.current")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("the cgroup's memory: %w", err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}
