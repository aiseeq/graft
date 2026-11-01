//go:build linux

package iosched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

const ioWeight = 10

var limitFiles = []string{"memory.max", "memory.high", "cpu.max"}

type plan struct {
	source, slice, scope string
	limited              string
}

// decide contains the environment-independent cgroup placement rules; sole
// says the process is the only one in its scope.
func decide(cgroup string, limits map[string]string, sole bool, pid int) (plan, bool) {
	p := path.Clean("/" + strings.TrimSpace(cgroup))
	parts := strings.Split(strings.Trim(p, "/"), "/")
	userManager := false
	for _, part := range parts {
		if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
			userManager = true
		}
	}
	if !userManager || len(parts) < 2 {
		return plan{}, false
	}
	unit := parts[len(parts)-1]
	if strings.HasSuffix(unit, ".service") || !strings.HasSuffix(unit, ".scope") {
		return plan{}, false
	}
	parent := parts[len(parts)-2]
	if !strings.HasSuffix(parent, ".slice") {
		return plan{}, false
	}
	if strings.HasSuffix(parent, "-graft.slice") {
		return plan{}, false
	}
	// Alone in its scope (systemd-run --scope graft …), the move would leave
	// the scope empty: systemd stops an empty scope, and BindsTo stops the
	// heavy scope with it, killing the task.
	if sole {
		return plan{}, false
	}
	for _, key := range limitFiles {
		// cpu.max is "$MAX $PERIOD": the quota alone says the limit.
		if value, ok := limits[key]; ok && firstField(value) != "max" {
			return plan{limited: key}, false
		}
	}
	newSlice := strings.TrimSuffix(parent, ".slice") + "-graft.slice"
	return plan{source: unit, slice: newSlice, scope: fmt.Sprintf("graft-heavy-%d.scope", pid)}, true
}

func scopeProperties(p plan, pid int) []string {
	return []string{"5", "PIDs", "au", "1", strconv.Itoa(pid), "Slice", "s", p.slice,
		"BindsTo", "as", "1", p.source, "After", "as", "1", p.source,
		"CollectMode", "s", "inactive-or-failed", "0"}
}

func sliceProperties() []string {
	return []string{"1", "IOWeight", "t", strconv.Itoa(ioWeight), "0"}
}

func enter(ctx context.Context, pid int, procRoot string) (string, error) {
	self := "/proc/self/cgroup"
	if procRoot != "" {
		self = path.Join(procRoot, "self", "cgroup")
	}
	data, err := os.ReadFile(self)
	if procRoot != "" && errors.Is(err, os.ErrNotExist) {
		// A stand-in machine without cgroups has no user manager to ask.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read current cgroup: %w", err)
	}
	var cg string
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			cg = fields[2]
			break
		}
	}
	if cg == "" {
		return "", nil
	}
	if _, ok := decide(cg, nil, false, pid); !ok {
		return "", nil
	}
	base := path.Join("/sys/fs/cgroup", cg)
	limits := make(map[string]string, 3)
	for _, key := range limitFiles {
		b, readErr := os.ReadFile(path.Join(base, key))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return "", fmt.Errorf("read cgroup limit %s: %w", key, readErr)
		}
		limits[key] = strings.TrimSpace(string(b))
	}
	procs, err := os.ReadFile(path.Join(base, "cgroup.procs"))
	if err != nil {
		return "", fmt.Errorf("read the processes of the scope: %w", err)
	}
	move, ok := decide(cg, limits, soleProcess(string(procs), pid), pid)
	if !ok {
		if move.limited != "" {
			return fmt.Sprintf("not moved: scope has %s limit", move.limited), nil
		}
		return "", nil
	}
	if err := ensureSlice(ctx, move.slice); err != nil {
		return "", err
	}
	if err := transient(ctx, move.scope, "fail", scopeProperties(move, pid)...); err != nil {
		return "", fmt.Errorf("create heavy scope %s: %w", move.scope, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, readErr := os.ReadFile(self)
		if readErr == nil && strings.Contains(string(current), "/"+move.scope) {
			return fmt.Sprintf("moved from %s to %s in %s (IOWeight=%d)", move.source, move.scope, move.slice, ioWeight), nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", fmt.Errorf("process did not enter heavy scope %s within 5s", move.scope)
}

// ensureSlice creates the shared slice with its weight, or, when it is
// already loaded (another graft made it first), sets the weight on it again.
func ensureSlice(ctx context.Context, name string) error {
	createErr := transient(ctx, name, "fail", sliceProperties()...)
	if createErr == nil {
		return nil
	}
	if err := manager(ctx, "SetUnitProperties", "sba(sv)", name, "true", "1", "IOWeight", "t", strconv.Itoa(ioWeight)); err != nil {
		return fmt.Errorf("create slice %s: %w", name, errors.Join(createErr, fmt.Errorf("set IOWeight: %w", err)))
	}
	return nil
}

func transient(ctx context.Context, name string, mode string, args ...string) error {
	return manager(ctx, "StartTransientUnit", "ssa(sv)a(sa(sv))", append([]string{name, mode}, args...)...)
}

// manager calls a method of the user systemd manager by busctl.
func manager(ctx context.Context, method, signature string, args ...string) error {
	all := append([]string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", method, signature}, args...)
	cmd := exec.CommandContext(ctx, "busctl", all...)
	if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		// busctl --user finds the user bus by these; the manager the cgroup
		// path names runs for this user, with its bus in the user's runtime
		// directory.
		cmd.Env = append(os.Environ(), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("busctl: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// firstField is the first word of a cgroup file's value, "" for none.
func firstField(value string) string {
	if fields := strings.Fields(value); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// soleProcess says whether pid is the only process cgroup.procs lists.
func soleProcess(procs string, pid int) bool {
	for line := range strings.SplitSeq(procs, "\n") {
		if line = strings.TrimSpace(line); line != "" && line != strconv.Itoa(pid) {
			return false
		}
	}
	return true
}
