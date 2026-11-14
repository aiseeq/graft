//go:build linux

package iosched

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHalfMode(t *testing.T) {
	oldPath, oldRead, oldNow, oldThreads := halfPath, halfRead, halfNow, halfThreads
	t.Cleanup(func() { halfPath, halfRead, halfNow, halfThreads = oldPath, oldRead, oldNow, oldThreads })
	until := time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC)
	halfPath = func() (string, error) { return "half-state", nil }
	halfRead = func(string) (time.Time, error) { return until, nil }
	halfNow = func() time.Time { return until.Add(-time.Minute) }
	halfThreads = func() int { return 7 }
	quota, procs, gotUntil, err := HalfMode()
	if err != nil || quota != 3_500_000 || procs != 4 || !gotUntil.Equal(until) {
		t.Fatalf("HalfMode = %d, %d, %s, %v; want 3500000, 4, %s", quota, procs, gotUntil, err, until)
	}
	halfNow = func() time.Time { return until }
	if quota, procs, gotUntil, err = HalfMode(); err != nil || quota != 0 || procs != 0 || !gotUntil.IsZero() {
		t.Errorf("expired HalfMode = %d, %d, %s, %v; want no mode", quota, procs, gotUntil, err)
	}
}

func TestDecide(t *testing.T) {
	const scope = "/user.slice/user-1000.slice/user@1000.service/agents.slice/console.scope"
	const session = "/user.slice/user-1000.slice/user@1000.service/agents.slice/agent-probe-77-r2.scope"
	tests := []struct {
		name, cgroup string
		limits       map[string]string
		sole         bool
		wantSlice    string
		wantSource   string
		wantLimited  string
	}{
		{name: "outside user manager", cgroup: "/system.slice/console.scope"},
		{name: "service", cgroup: "/user.slice/user@1000.service/agents.slice/agent.service"},
		{name: "already in graft slice", cgroup: "/user.slice/user@1000.service/agents-graft.slice/graft-heavy-1.scope"},
		{name: "memory max limit", cgroup: scope, limits: map[string]string{"memory.max": "1G"}, wantLimited: "memory.max"},
		{name: "memory high limit", cgroup: scope, limits: map[string]string{"memory.high": "2G"}, wantLimited: "memory.high"},
		{name: "cpu limit", cgroup: scope, limits: map[string]string{"cpu.max": "100000 100000"}, wantLimited: "cpu.max"},
		{name: "unlimited scope", cgroup: scope, limits: map[string]string{"memory.max": "max", "memory.high": "max", "cpu.max": "max 100000"}, wantSlice: "agents-graft.slice", wantSource: "console.scope"},
		{name: "cpu.max file absent", cgroup: scope, limits: map[string]string{"memory.max": "max", "memory.high": "max"}, wantSlice: "agents-graft.slice", wantSource: "console.scope"},
		{name: "alone in its scope", cgroup: scope, limits: map[string]string{}, sole: true},
		{name: "nested slice", cgroup: "/user.slice/user@1000.service/agents-x.slice/console.scope", limits: map[string]string{}, wantSlice: "agents-x-graft.slice", wantSource: "console.scope"},
		{name: "agent session with memory ceiling", cgroup: session, limits: map[string]string{"memory.max": "2G", "memory.high": "1800M"}, wantSlice: "agents-graft.slice", wantSource: "agent-probe-77-r2.scope"},
		{name: "agent session with cpu limit", cgroup: session, limits: map[string]string{"memory.max": "2G", "cpu.max": "100000 100000"}, wantLimited: "cpu.max"},
		{name: "agent-named scope outside agents.slice", cgroup: "/user.slice/user@1000.service/app.slice/agent-probe-77.scope", limits: map[string]string{"memory.max": "2G"}, wantLimited: "memory.max"},
		{name: "other scope in agents.slice", cgroup: "/user.slice/user@1000.service/agents.slice/probe-77.scope", limits: map[string]string{"memory.max": "2G"}, wantLimited: "memory.max"},
		{name: "agent-named scope without a pid", cgroup: "/user.slice/user@1000.service/agents.slice/agent-foreign.scope", limits: map[string]string{"memory.max": "2G"}, wantLimited: "memory.max"},
		{name: "agent session with swap ceiling only", cgroup: session, limits: map[string]string{"memory.swap.max": "1G"}, wantSlice: "agents-graft.slice", wantSource: "agent-probe-77-r2.scope"},
		{name: "swap ceiling only", cgroup: scope, limits: map[string]string{"memory.max": "max", "memory.swap.max": "0"}, wantSlice: "agents-graft.slice", wantSource: "console.scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, move := decide(tt.cgroup, tt.limits, tt.sole, 42, 7)
			if tt.wantSlice == "" && move {
				t.Fatalf("unexpected move: %+v", got)
			}
			if tt.wantSlice != "" && (!move || got.slice != tt.wantSlice || got.source != tt.wantSource || got.scope != "graft-heavy-42.scope") {
				t.Fatalf("decide() = %+v, move %v", got, move)
			}
			if got.limited != tt.wantLimited {
				t.Fatalf("limited = %q, want %q", got.limited, tt.wantLimited)
			}
			if move {
				want := []string{"9", "PIDs", "au", "1", "42", "Slice", "s", tt.wantSlice,
					"PartOf", "as", "1", tt.wantSource, "After", "as", "1", tt.wantSource,
					"CollectMode", "s", "inactive-or-failed", "IOAccounting", "b", "true",
					"MemoryMax", "t", "7", "MemorySwapMax", "t", "0", "OOMPolicy", "s", "continue", "0"}
				if properties := scopeProperties(got, 42); !reflect.DeepEqual(properties, want) {
					t.Fatalf("scope properties = %q, want %q", properties, want)
				}
			}
		})
	}
	if got, want := sliceProperties(2), []string{"2", "IOWeight", "t", "10", "MemoryMax", "t", "2", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("slice properties = %q, want %q", got, want)
	}
	if got := sliceCeiling(100); got != 50 {
		t.Errorf("slice ceiling = %d, want 50%% of RAM", got)
	}

}

func TestDecideSession(t *testing.T) {
	uid := os.Getuid()
	cg := "/user.slice/user-" + strconv.Itoa(uid) + ".slice/session-7.scope"
	move, ok := decideSession(cg, map[string]string{}, 42, 7, uid)
	if !ok || move.slice != "app-graft.slice" || move.source != "session-7.scope" || move.scope != "graft-heavy-42.scope" || move.linkSource {
		t.Fatalf("session plan = %+v, move %v", move, ok)
	}
	want := []string{"7", "PIDs", "au", "1", "42", "Slice", "s", "app-graft.slice", "CollectMode", "s", "inactive-or-failed", "IOAccounting", "b", "true",
		"MemoryMax", "t", "7", "MemorySwapMax", "t", "0", "OOMPolicy", "s", "continue", "0"}
	if got := scopeProperties(move, 42); !reflect.DeepEqual(got, want) {
		t.Fatalf("session scope properties = %q, want %q", got, want)
	}
	if got, ok := decideSession(cg, map[string]string{}, 42, 7, uid+1); ok || got.scope != "" {
		t.Errorf("foreign session scope planned: %+v, %v", got, ok)
	}
	if got, ok := decideSession(cg, map[string]string{"memory.max": "1G"}, 42, 7, uid); ok || got.limited != "memory.max" {
		t.Errorf("limited session scope = %+v, %v", got, ok)
	}
	if got := noUserManager(uid, false, false, "34% of RAM, the default"); !strings.Contains(got.Message, "no memory ceiling: no user manager runs") || !strings.Contains(got.Message, "uid "+strconv.Itoa(uid)) || strings.Contains(got.Message, "not applied") {
		t.Errorf("missing manager message = %q", got.Message)
	}
	if got := noUserManager(uid, false, true, "--memory-max 1G"); !strings.Contains(got.Message, "--memory-max 1G not applied") {
		t.Errorf("an asked ceiling is not named as not applied: %q", got.Message)
	}
	if got := noUserManager(uid, true, false, "34% of RAM, the default"); got.Message != "" {
		t.Errorf("a parent's memory.max holds the work, yet: %q", got.Message)
	}
	if got := noUserManager(uid, true, true, "--memory-max 1G"); strings.Contains(got.Message, "no memory ceiling") || !strings.Contains(got.Message, "--memory-max 1G not applied") {
		t.Errorf("a parent's memory.max with an asked ceiling: %q", got.Message)
	}
	root := t.TempDir()
	if running, err := hasUserManager(root, uid); err != nil || running {
		t.Fatalf("missing user manager cgroup detected: %v, %v", running, err)
	}
	manager := path.Join(root, "cgroup", "user.slice", "user-"+strconv.Itoa(uid)+".slice", "user@"+strconv.Itoa(uid)+".service")
	if err := os.MkdirAll(manager, 0o755); err != nil {
		t.Fatal(err)
	}
	if running, err := hasUserManager(root, uid); err != nil || !running {
		t.Fatalf("existing user manager cgroup not detected: %v, %v", running, err)
	}
}

func TestEnterSystemd(t *testing.T) {
	if testing.Short() {
		t.Skip("live systemd test is disabled in short mode")
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("cannot inspect current cgroup: %v", err)
	}
	var cg string
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			cg = fields[2]
		}
	}
	if _, ok := decide(cg, nil, false, os.Getpid(), 1); !ok {
		t.Skip("test process is not in a scope under a user manager")
	}
	base := path.Join("/sys/fs/cgroup", cg)
	limits, err := readLimits(base)
	if err != nil {
		t.Fatal(err)
	}
	procs, err := os.ReadFile(path.Join(base, "cgroup.procs"))
	if err != nil {
		t.Fatalf("cannot read the processes of the scope: %v", err)
	}
	if _, ok := decide(cg, limits, soleProcess(string(procs), os.Getpid()), os.Getpid(), 1); !ok {
		t.Skip("test process is alone in its scope or the scope has a limit")
	}
	place, err := Configure(context.Background(), os.Getpid(), "", Request{Value: "1G", Source: "memory_max of task test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(place.Message, "moved from") || place.Ceiling != 1<<30 {
		t.Fatalf("placement %+v, want a move under 1 GiB", place)
	}
	b, err = os.ReadFile("/proc/self/cgroup")
	if err != nil || !strings.Contains(string(b), "-graft.slice/graft-heavy-") {
		t.Fatalf("cgroup after the move %q, %v", b, err)
	}
	moved := path.Join("/sys/fs/cgroup", unifiedCgroup(string(b)))
	for file, want := range map[string]string{"memory.max": "1073741824", "memory.swap.max": "0"} {
		if got, err := os.ReadFile(path.Join(moved, file)); err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("%s = %q, %v; want %s", file, got, err, want)
		}
	}
	// A later heavy task of the process asking for more raises the ceiling.
	if place, err = Configure(context.Background(), os.Getpid(), "", Request{Value: "2G", Source: "memory_max of task more"}); err != nil || place.Ceiling != 2<<30 {
		t.Fatalf("raise: %+v, %v", place, err)
	}
	if got, err := os.ReadFile(path.Join(moved, "memory.max")); err != nil || strings.TrimSpace(string(got)) != "2147483648" {
		t.Errorf("memory.max after the raise = %q, %v", got, err)
	}
}

// TestWaitMovedLateStart: systemd runs the start job well after answering the
// call, and the process that waits for it goes on, with how long it took.
func TestWaitMovedLateStart(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the seconds a loaded manager takes")
	}
	self := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(self, []byte("0::/old.scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	late := slowEnter + 500*time.Millisecond
	timer := time.AfterFunc(late, func() {
		if err := os.WriteFile(self, []byte("0::/s/late.scope\n"), 0o600); err != nil {
			t.Error(err)
		}
	})
	defer timer.Stop()
	waited, err := waitMoved(context.Background(), self, "late.scope", enterWait)
	if err != nil {
		t.Fatal(err)
	}
	if waited < late {
		t.Fatalf("waited = %s, want at least %s", waited, late)
	}
}

func TestWaitMovedNotEntered(t *testing.T) {
	self := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(self, []byte("0::/old.scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := waitMoved(context.Background(), self, "never.scope", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never.scope within 100ms") {
		t.Fatalf("err = %v, want the scope and the limit", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitMoved(ctx, self, "never.scope", enterWait); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestUnitState: a wait that ends without the process in its scope says what
// the manager holds of the unit, here one it does not know.
func TestUnitState(t *testing.T) {
	if testing.Short() {
		t.Skip("talks to the user systemd manager")
	}
	got := unitState(context.Background(), "graft-no-such-unit.scope")
	if strings.Contains(got, "not read") {
		t.Skip(got)
	}
	if !strings.Contains(got, "LoadState=not-found") {
		t.Fatalf("unitState = %q, want LoadState=not-found", got)
	}
}

// TestUnitStateBounded: a manager that does not answer does not hold the
// failure report past the bound.
func TestUnitStateBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the bound of a look at a unit")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	start := time.Now()
	got := unitState(context.Background(), "any.scope")
	if took := time.Since(start); took > 2*stateWait {
		t.Fatalf("unitState took %s, bound %s", took, stateWait)
	}
	if !strings.Contains(got, "not read") {
		t.Fatalf("unitState = %q, want a state not read", got)
	}
}

func TestLateNote(t *testing.T) {
	if got := lateNote(slowEnter - time.Millisecond); got != "" {
		t.Errorf("in time: %q, want none", got)
	}
	if got := lateNote(6600 * time.Millisecond); !strings.Contains(got, "took 6.6s") {
		t.Errorf("late: %q, want the wait", got)
	}
}

// TestHolds: graft raises a scope it holds, sets a ceiling on a user scope it
// is alone in without limits, and leaves the rest to placement.
func TestHolds(t *testing.T) {
	const user = "/user.slice/user-1000.slice/user@1000.service/"
	for _, c := range []struct {
		name, cgroup, owned string
		limits              map[string]string
		sole                bool
		want                held
	}{
		{name: "heavy scope", cgroup: user + "agents-graft.slice/graft-heavy-7.scope", limits: map[string]string{"memory.max": "1073741824"}, want: heavyHeld},
		{name: "own sole scope with ceiling", cgroup: user + "app.slice/run-1.scope", owned: "run-1.scope", limits: map[string]string{"memory.max": "1073741824"}, sole: true, want: heavyHeld},
		{name: "alone without limits", cgroup: user + "app.slice/run-1.scope", limits: map[string]string{"memory.max": "max"}, sole: true, want: soleScope},
		{name: "alone with caller's limit", cgroup: user + "app.slice/run-1.scope", limits: map[string]string{"memory.max": "3221225472"}, sole: true, want: notHeld},
		{name: "alone with cpu limit", cgroup: user + "app.slice/run-1.scope", limits: map[string]string{"cpu.max": "100000 100000"}, sole: true, want: notHeld},
		{name: "alone in a session scope", cgroup: "/user.slice/user-1000.slice/session-3.scope", sole: true, want: notHeld},
		{name: "alone in a service", cgroup: user + "app.slice/worker-1.service", sole: true, want: notHeld},
		{name: "shared scope", cgroup: user + "agents.slice/agent-1.scope", want: notHeld},
		{name: "scope named like heavy outside the graft slice", cgroup: user + "agents.slice/graft-heavy-7.scope", want: notHeld},
		{name: "a parent graft's sole scope", cgroup: user + "app.slice/run-1.scope", owned: "run-1.scope", limits: map[string]string{"memory.max": "314572800"}, want: heavyHeld},
		{name: "a parent graft's scope, another one here", cgroup: user + "app.slice/run-2.scope", owned: "run-1.scope", limits: map[string]string{"memory.max": "314572800"}, want: notHeld},
	} {
		if got := holds(c.cgroup, c.limits, c.sole, "", c.owned); got != c.want {
			t.Errorf("%s: holds = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestRaise: the ceiling of a held scope goes up to a larger request, never
// down, and a scope without one gets it.
func TestRaise(t *testing.T) {
	var set []int64
	setCeiling = func(_ context.Context, _ string, ceiling int64) error { set = append(set, ceiling); return nil }
	t.Cleanup(func() { setCeiling = setScope })
	const cg = "/user.slice/user-1000.slice/user@1000.service/agents-graft.slice/graft-heavy-7.scope"
	for _, c := range []struct {
		current  string
		ceiling  int64
		explicit bool
		want     int64
		set      bool
	}{
		{"4294967296", 8 << 30, true, 8 << 30, true},
		{"8589934592", 4 << 30, true, 8 << 30, false},
		{"max", 4 << 30, true, 4 << 30, true},
		// The default leaves a lower ceiling a parent asked for, and sets
		// one where there is none.
		{"1073741824", 40 << 30, false, 1 << 30, false},
		{"max", 40 << 30, false, 40 << 30, true},
	} {
		set = nil
		p, err := raise(context.Background(), cg, map[string]string{"memory.max": c.current}, c.ceiling, "memory_max of task t 8G", 100<<30, c.explicit)
		if err != nil || p.Ceiling != c.want || (len(set) > 0) != c.set || p.Slice != "agents-graft.slice" || p.SliceMax != 50<<30 {
			t.Errorf("raise from %s to %d: %+v, %v, set %v", c.current, c.ceiling, p, err, set)
		}
	}
	if _, err := raise(context.Background(), cg, map[string]string{"memory.max": "lots"}, 1, "", 100, true); err == nil {
		t.Error("raise accepted an unreadable memory.max")
	}
}

// TestCeilingOf: the default is a share of RAM; a request is resolved and
// cited with its source.
func TestCeilingOf(t *testing.T) {
	if got, source, err := ceilingOf(Request{}, 100<<30); err != nil || got != DefaultPercent<<30 || !strings.Contains(source, "default") {
		t.Errorf("default = %d, %q, %v", got, source, err)
	}
	if got, source, err := ceilingOf(Request{Value: "12G", Source: "--memory-max"}, 100<<30); err != nil || got != 12<<30 || source != "--memory-max 12G" {
		t.Errorf("12G = %d, %q, %v", got, source, err)
	}
	if _, _, err := ceilingOf(Request{Value: "12GB", Source: "--memory-max"}, 100<<30); err == nil {
		t.Error("12GB accepted")
	}
	// The graft exec that started this graft asked for more or less: the
	// larger holds, with its source.
	outer := Request{Value: "300M", Source: "--memory-max", Outer: "1G", OuterSource: "outer"}
	if got, source, err := ceilingOf(outer, 100<<30); err != nil || got != 1<<30 || source != "outer 1G" {
		t.Errorf("300M inside 1G = %d, %q, %v", got, source, err)
	}
	outer.Value, outer.Outer = "1G", "300M"
	if got, source, err := ceilingOf(outer, 100<<30); err != nil || got != 1<<30 || source != "--memory-max 1G" {
		t.Errorf("1G inside 300M = %d, %q, %v", got, source, err)
	}
	if got, _, err := ceilingOf(Request{Outer: "45%", OuterSource: "outer"}, 100<<30); err != nil || got != 45<<30 {
		t.Errorf("45%% outer, none here = %d, %v", got, err)
	}
	if _, _, err := ceilingOf(Request{Outer: "x", OuterSource: "outer"}, 100<<30); err == nil {
		t.Error("bad outer accepted")
	}
}

// TestEnterStandInMachine: a test's stand-in /proc without a cgroup file has
// no user manager, so nothing moves and nothing fails.
func TestEnterStandInMachine(t *testing.T) {
	place, err := Configure(context.Background(), os.Getpid(), t.TempDir(), Request{})
	if err != nil || place != (Placement{}) {
		t.Fatalf("Configure = %+v, %v; want nothing", place, err)
	}
}

// TestSoleProcess: only the process itself in cgroup.procs is alone.
func TestSoleProcess(t *testing.T) {
	for _, c := range []struct {
		procs string
		want  bool
	}{{"42\n", true}, {"", true}, {"7\n42\n", false}, {"42\n43\n", false}} {
		if got := soleProcess(c.procs, 42); got != c.want {
			t.Errorf("soleProcess(%q) = %v, want %v", c.procs, got, c.want)
		}
	}
}

// TestScopePropertiesCount: the count busctl reads first is the number of
// properties that follow, with and without the link to the source scope; a
// wrong count fails StartTransientUnit with too few parameters.
func TestHalfModeQuotaProperty(t *testing.T) {
	until := time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC)
	if got, want := cpuCeilingNote(3_500_000, until), ", CPU ceiling 3.5 threads: half until 10:30"; got != want {
		t.Errorf("CPU ceiling note = %q, want %q", got, want)
	}
	if got := cpuCeilingNote(0, until); got != "" {
		t.Errorf("no-quota note = %q, want empty", got)
	}
	p := plan{source: "console.scope", slice: "agents-graft.slice", scope: "graft-heavy-42.scope", ceiling: 7, linkSource: true, cpuQuotaUSec: 3_500_000, halfUntil: time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC)}
	got := scopeProperties(p, 42)
	if !strings.Contains(strings.Join(got, " "), "CPUQuotaPerSecUSec t 3500000") {
		t.Fatalf("scope properties lack half CPU quota: %q", got)
	}
	if got[0] != "10" {
		t.Errorf("property count = %s, want 10", got[0])
	}
}

func TestScopePropertiesCount(t *testing.T) {
	names := map[string]bool{"PIDs": true, "Slice": true, "PartOf": true, "After": true, "CollectMode": true, "IOAccounting": true, "MemoryMax": true, "MemorySwapMax": true, "OOMPolicy": true}
	for _, linked := range []bool{true, false} {
		p := plan{source: "app-x.scope", slice: "app-graft.slice", scope: "graft-heavy-42.scope", ceiling: 7, linkSource: linked}
		properties := scopeProperties(p, 42)
		found := 0
		for _, word := range properties[1:] {
			if names[word] {
				found++
			}
		}
		if properties[0] != strconv.Itoa(found) {
			t.Errorf("linked %v: count %s, %d properties follow: %q", linked, properties[0], found, properties)
		}
	}
}
