// Package iosched places admitted heavy work in a shared, low-weight cgroup
// under a memory ceiling.
package iosched

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"github.com/aiseeq/akit/pressure"
)

// DefaultPercent is the memory ceiling of heavy work, in percent of RAM, when
// the task asks for none: at least twice the largest peak of a heavy task
// that asks for none, and under the free memory a busy machine has 99% of the
// time. SlicePercent is the ceiling of all heavy work of a shared slice
// together.
const (
	DefaultPercent = 34
	SlicePercent   = 50
)

var (
	halfPath    = pressure.HalfPath
	halfRead    = pressure.ReadQuiet
	halfNow     = time.Now
	halfThreads = runtime.NumCPU
)

// halfCPU follows pressure.Limits.RunnableLimit in akit/pressure:
// max(1, float64(threads)/2), preserving a fractional half on odd machines.
func halfCPU(threads int) float64 { return math.Max(1, float64(threads)/2) }

// HalfMode reports the active half-mode CPU allowance, if any. The integer
// GOMAXPROCS rounds the same allowance up; the quota retains its fraction.
func HalfMode() (quotaUSec int64, gomaxprocs int, until time.Time, err error) {
	p, err := halfPath()
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	until, err = halfRead(p)
	if err != nil || !until.After(halfNow()) {
		return 0, 0, time.Time{}, err
	}
	half := halfCPU(halfThreads())
	return int64(half * 1_000_000), int(math.Ceil(half)), until, nil
}

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
	Kind               string
	CPUQuotaUSec       int64
	HalfUntil          time.Time
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
