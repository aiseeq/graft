// Package iosched places admitted heavy work in a shared, low-weight cgroup
// under a memory ceiling.
package iosched

import (
	"context"
	"fmt"
)

// DefaultPercent is the memory ceiling of heavy work, in percent of RAM, when
// the task asks for none; SlicePercent is the ceiling of all heavy work of a
// shared slice together.
const (
	DefaultPercent = 40
	SlicePercent   = 50
)

// Request is the memory ceiling heavy work asks for: Value as memory_max
// takes it ("12G", "40%"), "" for the default; Source names where it was set
// (memory_max of task X, --memory-max), for the lines that cite it. Outer is
// the ceiling of the graft exec --heavy that started this graft, with its
// OuterSource: the larger of the two holds. Held is the scope a parent graft
// holds the work in: this graft may raise its ceiling.
type Request struct {
	Value, Source      string
	Outer, OuterSource string
	Held               string
}

// explicit says the request names a ceiling, not the default.
func (r Request) explicit() bool { return r.Value != "" || r.Outer != "" }

// Placement is what Configure did with the cgroup of the heavy work.
type Placement struct {
	// Message is the line for the user, "" for none.
	Message string
	// Ceiling is the memory.max graft holds the work under and Source what
	// set it; 0 when graft set none.
	Ceiling int64
	Source  string
	// Scope is the unit the ceiling is on; Slice, when set, the shared
	// slice holding it, under SliceMax.
	Scope    string
	Slice    string
	SliceMax int64
	// NoCeiling says no memory.max holds the work: graft set none, and
	// neither its cgroup nor a parent has one.
	NoCeiling bool
}

// Configure moves the current process into the shared heavy-work slice when
// its current systemd scope can safely be moved, and gives the scope the
// requested memory ceiling; a scope graft already holds only gets it raised.
// Other platforms do nothing. procRoot, when set, stands for /proc and
// /sys/fs/cgroup (procRoot/cgroup) of a test's machine: without a cgroup file
// there it does nothing.
func Configure(ctx context.Context, pid int, procRoot string, req Request) (Placement, error) {
	return configure(ctx, pid, procRoot, req)
}

// Bytes is n for the user: GiB with one decimal, MiB below a GiB.
func Bytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
}
