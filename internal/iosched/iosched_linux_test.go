//go:build linux

package iosched

import (
	"context"
	"errors"
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
			got, move := decide(tt.cgroup, tt.limits, tt.sole, 42)
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
				want := []string{"5", "PIDs", "au", "1", "42", "Slice", "s", tt.wantSlice,
					"BindsTo", "as", "1", tt.wantSource, "After", "as", "1", tt.wantSource,
					"CollectMode", "s", "inactive-or-failed", "0"}
				if properties := scopeProperties(got, 42); !reflect.DeepEqual(properties, want) {
					t.Fatalf("scope properties = %q, want %q", properties, want)
				}
			}
		})
	}
	if got, want := sliceProperties(), []string{"1", "IOWeight", "t", "10", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("slice properties = %q, want %q", got, want)
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
	if _, ok := decide(cg, nil, false, os.Getpid()); !ok {
		t.Skip("test process is not in a scope under a user manager")
	}
	base := path.Join("/sys/fs/cgroup", cg)
	limits := map[string]string{}
	for _, key := range limitFiles {
		value, readErr := os.ReadFile(path.Join(base, key))
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			t.Fatalf("cannot inspect current scope limit %s: %v", key, readErr)
		}
		limits[key] = strings.TrimSpace(string(value))
	}
	if _, ok := decide(cg, limits, false, os.Getpid()); !ok {
		t.Skip("test process is not in an unlimited scope under a user manager")
	}
	message, err := Enter(context.Background(), os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "moved from") {
		t.Fatalf("message %q, want a move", message)
	}
	b, err = os.ReadFile("/proc/self/cgroup")
	if err != nil || !strings.Contains(string(b), "-graft.slice/graft-heavy-") {
		t.Fatalf("cgroup after the move %q, %v", b, err)
	}
}

// TestEnterStandInMachine: a test's stand-in /proc without a cgroup file has
// no user manager, so nothing moves and nothing fails.
func TestEnterStandInMachine(t *testing.T) {
	message, err := Enter(context.Background(), os.Getpid(), t.TempDir())
	if err != nil || message != "" {
		t.Fatalf("Enter = %q, %v; want nothing", message, err)
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
