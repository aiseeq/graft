package iosched

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/aiseeq/akit/pressure"
)

var (
	halfPath    = pressure.HalfPath
	halfRead    = pressure.ReadQuiet
	halfNow     = time.Now
	halfThreads = statThreads
)

// statThreads counts the machine's threads the way akit/pressure does: the
// cpuN lines of /proc/stat, not the CPU affinity of this process, so a graft
// started under taskset still caps at half the machine akit measures.
func statThreads() (int, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("threads of the machine: %w", err)
	}
	_, threads, err := pressure.ParseStat(string(data))
	if err != nil {
		return 0, fmt.Errorf("threads of the machine: %w", err)
	}
	return threads, nil
}

// HalfMode reports the active half-mode CPU allowance, if any: the runnable
// limit akit/pressure applies in half mode (half the threads, at least 1, a
// fraction kept on odd machines). The integer GOMAXPROCS rounds it up; the
// quota keeps the fraction.
func HalfMode() (quotaUSec int64, gomaxprocs int, until time.Time, err error) {
	p, err := halfPath()
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	until, err = halfRead(p)
	if err != nil || !until.After(halfNow()) {
		return 0, 0, time.Time{}, err
	}
	threads, err := halfThreads()
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	half := pressure.Limits{}.RunnableLimit(pressure.Load{Threads: threads, HalfUntil: until})
	return int64(half * 1_000_000), int(math.Ceil(half)), until, nil
}
