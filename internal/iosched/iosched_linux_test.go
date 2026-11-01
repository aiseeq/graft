//go:build linux

package iosched

import (
	"context"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	const scope = "/user.slice/user-1000.slice/user@1000.service/agents.slice/console.scope"
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
				want := []string{"8", "PIDs", "au", "1", "42", "Slice", "s", tt.wantSlice,
					"PartOf", "as", "1", tt.wantSource, "After", "as", "1", tt.wantSource,
					"CollectMode", "s", "inactive-or-failed",
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
