package testdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ErrNoCgroup: the container's cgroup is out of reach, as with docker in a
// virtual machine (Docker Desktop) or cgroup v1; nothing is wrong.
var ErrNoCgroup = errors.New("the container's cgroup v2 is not visible from this host")

// cgroupRoot is where the cgroup v2 hierarchy is mounted.
const cgroupRoot = "/sys/fs/cgroup"

// Cgroup is the directory of the running container's cgroup v2, where the
// CPU and memory of PostgreSQL and every backend it forks are counted.
func (d *DB) Cgroup(ctx context.Context) (string, error) {
	if runtime.GOOS != "linux" {
		return "", ErrNoCgroup
	}
	s, err := d.Inspect(ctx)
	if err != nil {
		return "", err
	}
	if s.Refused != nil {
		return "", ErrNoCgroup // docker, which knows the server's process, is out of reach
	}
	if !s.Running || s.Pid == 0 {
		return "", fmt.Errorf("%s is not running", d.Cfg.Container)
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(s.Pid), "cgroup"))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCgroup // the process lives in another machine
	}
	if err != nil {
		return "", fmt.Errorf("the container's cgroup: %w", err)
	}
	rel, err := unifiedPath(string(data))
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cgroupRoot, filepath.FromSlash(rel))
	if _, err := os.Stat(filepath.Join(dir, "cpu.stat")); err != nil {
		return "", fmt.Errorf("%s: %w", d.Cfg.Container, err)
	}
	return dir, nil
}

// unifiedPath is the cgroup v2 path in a /proc/<pid>/cgroup file: its
// "0::<path>" line.
func unifiedPath(procCgroup string) (string, error) {
	for line := range strings.Lines(procCgroup) {
		if p, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "0::"); ok {
			return p, nil
		}
	}
	return "", ErrNoCgroup
}
