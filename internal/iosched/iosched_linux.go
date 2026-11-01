//go:build linux

package iosched

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/memorymax"
)

const ioWeight = 10

var limitFiles = []string{"memory.max", "memory.high", "cpu.max"}

// ownScope is the scope this process found itself alone in and gave a
// ceiling: a later heavy task of the process may raise it as a heavy scope's.
var ownScope string

// setCeiling gives a loaded scope a memory ceiling; tests stand in for it.
var setCeiling = setScope

type plan struct {
	source, slice, scope string
	limited              string
	ceiling              int64
}

// userScope says whether cgroup is a scope of a user's systemd manager, and
// gives its unit and parent slice.
func userScope(cgroup string) (unit, parent string, ok bool) {
	p := path.Clean("/" + strings.TrimSpace(cgroup))
	parts := strings.Split(strings.Trim(p, "/"), "/")
	userManager := false
	for _, part := range parts {
		if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
			userManager = true
		}
	}
	if !userManager || len(parts) < 2 {
		return "", "", false
	}
	unit, parent = parts[len(parts)-1], parts[len(parts)-2]
	if !strings.HasSuffix(unit, ".scope") || !strings.HasSuffix(parent, ".slice") {
		return "", "", false
	}
	return unit, parent, true
}

// decide contains the environment-independent cgroup placement rules; sole
// says the process is the only one in its scope.
func decide(cgroup string, limits map[string]string, sole bool, pid int, ceiling int64) (plan, bool) {
	unit, parent, ok := userScope(cgroup)
	if !ok || strings.HasSuffix(parent, "-graft.slice") {
		return plan{}, false
	}
	// Alone in its scope (systemd-run --scope graft …), the move would leave
	// the scope empty: systemd drops an empty scope, and the caller's handle
	// on the work (stopping its scope) would no longer reach it.
	if sole {
		return plan{}, false
	}
	if key := limitedBy(limits); key != "" {
		return plan{limited: key}, false
	}
	newSlice := strings.TrimSuffix(parent, ".slice") + "-graft.slice"
	return plan{source: unit, slice: newSlice, scope: fmt.Sprintf("graft-heavy-%d.scope", pid), ceiling: ceiling}, true
}

// limitedBy is the first limit file that limits, "" for none.
func limitedBy(limits map[string]string) string {
	for _, key := range limitFiles {
		// cpu.max is "$MAX $PERIOD": the quota alone says the limit.
		if value, ok := limits[key]; ok && firstField(value) != "max" {
			return key
		}
	}
	return ""
}

type held int

const (
	notHeld   held = iota // graft holds no ceiling on this scope
	soleScope             // alone in a user scope without limits: set the ceiling there
	heavyHeld             // a scope graft gave a ceiling: raise it, never lower
)

// holds says how graft holds the ceiling of the scope cgroup names; owned
// are the scopes this process or a parent graft gave a ceiling (a scope it
// was alone in included).
func holds(cgroup string, limits map[string]string, sole bool, owned ...string) held {
	unit, parent, ok := userScope(cgroup)
	if !ok {
		return notHeld
	}
	if strings.HasSuffix(parent, "-graft.slice") && strings.HasPrefix(unit, "graft-heavy-") || slices.Contains(owned, unit) {
		return heavyHeld
	}
	if sole && limitedBy(limits) == "" {
		return soleScope
	}
	return notHeld
}

// scopeProperties make the heavy scope PartOf its source: stopping the source
// scope stops the work, as when it ran there, while the source emptying on its
// own (the caller exits, the work goes on) leaves it running. BindsTo would
// stop it then too. Without OOMPolicy=continue, systemd would stop the whole
// scope, graft with it, when the kernel kills a process at the ceiling.
func scopeProperties(p plan, pid int) []string {
	return append([]string{"8", "PIDs", "au", "1", strconv.Itoa(pid), "Slice", "s", p.slice,
		"PartOf", "as", "1", p.source, "After", "as", "1", p.source,
		"CollectMode", "s", "inactive-or-failed"}, append(memoryProperties(p.ceiling), "0")...)
}

// memoryProperties hold the work under ceiling without swap: swapping out a
// runaway's memory is what stalls the machine. OOMPolicy goes only on a new
// scope: systemd refuses to set it on a running one.
func memoryProperties(ceiling int64) []string {
	return []string{"MemoryMax", "t", strconv.FormatInt(ceiling, 10), "MemorySwapMax", "t", "0", "OOMPolicy", "s", "continue"}
}

// ceilingProperties are memoryProperties a running scope takes.
func ceilingProperties(ceiling int64) []string {
	return memoryProperties(ceiling)[:6]
}

func sliceProperties(max int64) []string {
	return []string{"2", "IOWeight", "t", strconv.Itoa(ioWeight), "MemoryMax", "t", strconv.FormatInt(max, 10), "0"}
}

func sliceCeiling(total int64) int64 { return total * SlicePercent / 100 }

// ceilingOf resolves the request against total RAM, the larger of its own
// value and the outer one, and says where it came from. The ceiling is whole
// pages, as systemd keeps it, so that a later request for the same compares
// equal.
func ceilingOf(req Request, total int64) (int64, string, error) {
	page := int64(os.Getpagesize())
	if !req.explicit() {
		return total / 100 * DefaultPercent / page * page, fmt.Sprintf("%d%% of RAM, the default", DefaultPercent), nil
	}
	best, source := int64(0), ""
	for _, c := range []struct{ value, source string }{{req.Value, req.Source}, {req.Outer, req.OuterSource}} {
		if c.value == "" {
			continue
		}
		ceiling, err := memorymax.Resolve(c.value, total)
		if err != nil {
			return 0, "", fmt.Errorf("%s %q: %w", c.source, c.value, err)
		}
		if ceiling > best {
			best, source = ceiling, c.source+" "+c.value
		}
	}
	return max(best/page*page, page), source, nil
}

func configure(ctx context.Context, pid int, procRoot string, req Request) (Placement, error) {
	self := "/proc/self/cgroup"
	if procRoot != "" {
		self = path.Join(procRoot, "self", "cgroup")
	}
	data, err := os.ReadFile(self)
	if procRoot != "" && errors.Is(err, os.ErrNotExist) {
		// A stand-in machine without cgroups has no user manager to ask.
		return Placement{}, nil
	}
	if err != nil {
		return Placement{}, fmt.Errorf("read current cgroup: %w", err)
	}
	cg := unifiedCgroup(string(data))
	if cg == "" {
		return Placement{}, nil
	}
	total, err := memorymax.Total(procRoot)
	if err != nil {
		return Placement{}, err
	}
	ceiling, source, err := ceilingOf(req, total)
	if err != nil {
		return Placement{}, err
	}
	base := cgroupBase(procRoot, cg)
	limits, err := readLimits(base)
	if err != nil {
		return Placement{}, err
	}
	procs, err := os.ReadFile(path.Join(base, "cgroup.procs"))
	if err != nil {
		return Placement{}, fmt.Errorf("read the processes of the scope: %w", err)
	}
	sole := soleProcess(string(procs), pid)
	switch holds(cg, limits, sole, ownScope, req.Held) {
	case heavyHeld:
		return raise(ctx, cg, limits, ceiling, source, total, req.explicit())
	case soleScope:
		return holdSole(ctx, path.Base(cg), ceiling, source)
	}
	move, ok := decide(cg, limits, sole, pid, ceiling)
	if !ok {
		return unplaced(procRoot, cg, move.limited, req.explicit(), source)
	}
	sliceMax := sliceCeiling(total)
	if err := ensureSlice(ctx, move.slice, sliceMax); err != nil {
		return Placement{}, err
	}
	if err := transient(ctx, move.scope, "fail", scopeProperties(move, pid)...); err != nil {
		return Placement{}, fmt.Errorf("create heavy scope %s: %w", move.scope, err)
	}
	if err := waitMoved(ctx, self, move.scope); err != nil {
		return Placement{}, err
	}
	return Placement{
		Message: fmt.Sprintf("moved from %s to %s in %s (IOWeight=%d, memory ceiling %s: %s)", move.source, move.scope, move.slice, ioWeight, Bytes(ceiling), source),
		Ceiling: ceiling, Source: source, Scope: move.scope, Slice: move.slice, SliceMax: sliceMax,
	}, nil
}

// holdSole gives the scope graft is alone in the ceiling and says, when its
// OOMPolicy is not continue, that a kill at it stops graft too.
func holdSole(ctx context.Context, unit string, ceiling int64, source string) (Placement, error) {
	if err := setCeiling(ctx, unit, ceiling); err != nil {
		return Placement{}, fmt.Errorf("set memory ceiling on scope %s: %w", unit, err)
	}
	ownScope = unit
	message := fmt.Sprintf("alone in %s: memory ceiling %s (%s) set on it", unit, Bytes(ceiling), source)
	policy, err := oomPolicy(ctx, unit)
	if err != nil {
		return Placement{}, err
	}
	if policy != "continue" {
		message += fmt.Sprintf("; with its OOMPolicy=%s, the first kill at the ceiling also stops the scope, graft with it (systemd-run -p OOMPolicy=continue keeps it running)", policy)
	}
	return Placement{Message: message, Ceiling: ceiling, Source: source, Scope: unit}, nil
}

// unplaced is the placement of work graft neither moves nor holds: limited
// names the limit of its scope, "" for none; explicit says the ceiling source
// names was asked for, and so is said not applied.
func unplaced(procRoot, cg, limited string, explicit bool, source string) (Placement, error) {
	skip := ""
	if explicit {
		skip = fmt.Sprintf("; %s not applied", source)
	}
	if limited != "" {
		return Placement{Message: fmt.Sprintf("not moved: scope has %s limit%s", limited, skip)}, nil
	}
	held, err := memoryLimited(procRoot, cg)
	switch {
	case err != nil:
		return Placement{}, err
	case held && !explicit:
		return Placement{}, nil
	case held:
		return Placement{Message: fmt.Sprintf("memory ceiling %s not applied: graft cannot place %s, and memory.max of it or a parent holds it", source, cg)}, nil
	}
	return Placement{Message: fmt.Sprintf("no memory ceiling: graft cannot place %s, and neither it nor a parent has memory.max%s", cg, skip)}, nil
}

// raise lifts the ceiling of a scope graft holds to ceiling when it is
// lower, and keeps it otherwise, silently.
func raise(ctx context.Context, cg string, limits map[string]string, ceiling int64, source string, total int64, explicit bool) (Placement, error) {
	unit, parent, _ := userScope(cg)
	p := Placement{Scope: unit}
	if strings.HasSuffix(parent, "-graft.slice") {
		p.Slice, p.SliceMax = parent, sliceCeiling(total)
	}
	current := int64(0)
	if value := firstField(limits["memory.max"]); value != "" && value != "max" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Placement{}, fmt.Errorf("memory.max of %s: %q is not a number of bytes", unit, value)
		}
		current = n
	}
	// The default does not raise a ceiling a parent graft set: the parent's
	// own request, lower than the default, holds the work it starts through
	// any wrapper.
	if current >= ceiling || current > 0 && !explicit {
		p.Ceiling, p.Source = current, "the ceiling the heavy scope already has"
		return p, nil
	}
	if err := setCeiling(ctx, unit, ceiling); err != nil {
		return Placement{}, fmt.Errorf("raise the memory ceiling of %s: %w", unit, err)
	}
	p.Ceiling, p.Source = ceiling, source
	p.Message = fmt.Sprintf("memory ceiling of %s raised to %s (%s)", unit, Bytes(ceiling), source)
	return p, nil
}

// memoryLimited says whether the cgroup or a parent of it has memory.max.
func memoryLimited(procRoot, cg string) (bool, error) {
	for dir := path.Clean("/" + cg); ; dir = path.Dir(dir) {
		b, err := os.ReadFile(filepath.Join(cgroupBase(procRoot, dir), "memory.max"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("read cgroup limit memory.max: %w", err)
		}
		if err == nil && firstField(string(b)) != "max" {
			return true, nil
		}
		if dir == "/" {
			return false, nil
		}
	}
}

// unifiedCgroup is the cgroup v2 path of a /proc/<pid>/cgroup text, "" for
// none.
func unifiedCgroup(data string) string {
	for line := range strings.SplitSeq(data, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			return fields[2]
		}
	}
	return ""
}

// cgroupBase is the directory of cgroup cg: under /sys/fs/cgroup, or under
// procRoot/cgroup on a test's machine.
func cgroupBase(procRoot, cg string) string {
	root := "/sys/fs/cgroup"
	if procRoot != "" {
		root = filepath.Join(procRoot, "cgroup")
	}
	return filepath.Join(root, filepath.FromSlash(cg))
}

// readLimits reads the limit files of the cgroup at base; a file the cgroup
// lacks (its controller is off there) is no limit.
func readLimits(base string) (map[string]string, error) {
	limits := make(map[string]string, len(limitFiles))
	for _, key := range limitFiles {
		b, err := os.ReadFile(path.Join(base, key))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read cgroup limit %s: %w", key, err)
		}
		limits[key] = strings.TrimSpace(string(b))
	}
	return limits, nil
}

// waitMoved waits up to 5 seconds for the process's cgroup file self to name
// the new scope: systemd moves it when the start job runs.
func waitMoved(ctx context.Context, self, scope string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := os.ReadFile(self)
		if err == nil && strings.Contains(string(current), "/"+scope) {
			return nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("process did not enter heavy scope %s within 5s", scope)
}

// ensureSlice creates the shared slice with its weight and ceiling, or, when
// it is already loaded (another graft made it first), sets them on it again.
func ensureSlice(ctx context.Context, name string, max int64) error {
	createErr := transient(ctx, name, "fail", sliceProperties(max)...)
	if createErr == nil {
		return nil
	}
	if err := manager(ctx, "SetUnitProperties", "sba(sv)", name, "true", "2", "IOWeight", "t", strconv.Itoa(ioWeight), "MemoryMax", "t", strconv.FormatInt(max, 10)); err != nil {
		return fmt.Errorf("create slice %s: %w", name, errors.Join(createErr, fmt.Errorf("set IOWeight and MemoryMax: %w", err)))
	}
	return nil
}

// oomPolicy is the OOMPolicy of a loaded user unit.
func oomPolicy(ctx context.Context, unit string) (string, error) {
	out, err := userCommand(ctx, "systemctl", "--user", "show", "--property=OOMPolicy", "--value", unit).Output()
	if err != nil {
		return "", fmt.Errorf("read OOMPolicy of %s: %w", unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// setScope gives a running scope the memory ceiling, for this boot only.
func setScope(ctx context.Context, name string, ceiling int64) error {
	return manager(ctx, "SetUnitProperties", "sba(sv)", append([]string{name, "true", "2"}, ceilingProperties(ceiling)...)...)
}

// MemoryEvents reads memory.events of the process's current cgroup: oom
// counts reaching its own memory.max, oom_kill the processes in it the kernel
// killed for any limit or for lack of machine memory.
func MemoryEvents(procRoot string) (map[string]uint64, error) {
	self := "/proc/self/cgroup"
	if procRoot != "" {
		self = filepath.Join(procRoot, "self", "cgroup")
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return nil, fmt.Errorf("read current cgroup: %w", err)
	}
	cg := unifiedCgroup(string(b))
	if cg == "" {
		return nil, fmt.Errorf("no cgroup v2 line in %s", self)
	}
	b, err = os.ReadFile(filepath.Join(cgroupBase(procRoot, cg), "memory.events"))
	if err != nil {
		return nil, fmt.Errorf("read memory.events: %w", err)
	}
	out := map[string]uint64{}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("memory.events: %q: %w", line, err)
		}
		out[f[0]] = n
	}
	return out, nil
}

func transient(ctx context.Context, name string, mode string, args ...string) error {
	return manager(ctx, "StartTransientUnit", "ssa(sv)a(sa(sv))", append([]string{name, mode}, args...)...)
}

// manager calls a method of the user systemd manager by busctl.
func manager(ctx context.Context, method, signature string, args ...string) error {
	all := append([]string{"--user", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager", method, signature}, args...)
	if out, err := userCommand(ctx, "busctl", all...).CombinedOutput(); err != nil {
		return fmt.Errorf("busctl: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// userCommand is a command that talks to the user's systemd manager.
func userCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if os.Getenv("XDG_RUNTIME_DIR") == "" && os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		// --user finds the user bus by these; the manager the cgroup path
		// names runs for this user, with its bus in the user's runtime
		// directory.
		cmd.Env = append(os.Environ(), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	return cmd
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
