package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/checks"
	"github.com/aiseeq/graft/v2/internal/gocache"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

const maxDirtyWritebackKB = 64 * 1024
const trimKind = "gocache-trim"

// dirtyMeasured is whether the machine reports the memory queued to write:
// Linux does in /proc/meminfo, and a test's GRAFT_PROC stands for it.
// Elsewhere a batch waits for the busy check alone.
var dirtyMeasured = func() bool {
	return runtime.GOOS == "linux" || os.Getenv(loadgate.ProcEnv) != ""
}

// GocacheTrim removes, with apply, the entries of the Go build cache of
// go env GOCACHE not used for longer than older, or tells how many it
// would; it needs no project.
func (a *App) GocacheTrim(ctx context.Context, older time.Duration, apply bool, maxWait time.Duration) error {
	cmd := exec.CommandContext(ctx, "go", "env", "GOCACHE")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go env GOCACHE: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "off" || !filepath.IsAbs(dir) {
		return fmt.Errorf("go env GOCACHE is %q: no build cache to trim", dir)
	}
	waits := loadgate.Waits{}
	remainingWait := maxWait
	var options gocache.Options
	options.Apply = apply
	if apply {
		load, err := userconfig.LoadSettings()
		if err != nil {
			return fmt.Errorf("load machine pressure settings: %w", err)
		}
		checker := loadgate.NewDiskBusyChecker("gocache trim", trimKind, load, a.Stderr)
		options.BeforeBatch = func() (bool, error) {
			ok, batchWaits, err := waitForTrimBatch(ctx, remainingWait, load.Poll,
				trimMemoryCalm,
				checker.Why)
			for why, d := range batchWaits {
				waits[why] += d
			}
			remainingWait = max(0, remainingWait-waitTotal(batchWaits))
			return ok, err
		}
	}
	r, err := gocache.TrimWithOptions(dir, older, time.Now(), options)
	if err != nil {
		return err
	}
	verb := "removed"
	if !apply {
		verb = "dry run: graft gocache trim --apply removes"
	}
	if apply {
		waited := waitTotal(waits)
		a.printf("%s %d entries, %s, unused for %s in %s; %d entries, %s remain; %d entries, %s kept; waited %s%s",
			verb, r.Removed, checks.FormatSize(r.RemovedBytes), older, dir, r.Remaining, checks.FormatSize(r.RemainingBytes),
			r.Kept, checks.FormatSize(r.KeptBytes), waited.Round(time.Second), formatTrimWaits(waits))
	} else {
		a.printf("%s %d entries, %s, unused for %s in %s; %d entries, %s kept",
			verb, r.Removed, checks.FormatSize(r.RemovedBytes), older, dir, r.Kept, checks.FormatSize(r.KeptBytes))
	}
	return nil
}

func trimMemoryCalm() (bool, error) {
	if !dirtyMeasured() {
		return true, nil
	}
	dirtyWriteback, err := loadgate.DirtyWritebackKB()
	if err != nil {
		return false, err
	}
	return dirtyWriteback < maxDirtyWritebackKB, nil
}

func waitForTrimBatch(ctx context.Context, maxWait, poll time.Duration, dirty func() (bool, error), why func(context.Context) (string, error)) (bool, loadgate.Waits, error) {
	waits := loadgate.Waits{}
	// The budget runs on the clock, so a wait is the whole time since the
	// last one ended, the checks with it, not only the sleep between them.
	started := time.Now()
	deadline := started.Add(maxWait)
	charge := func(why string) {
		now := time.Now()
		waits[why] += now.Sub(started)
		started = now
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, waits, err
		}
		calm, err := dirty()
		if err != nil {
			return false, waits, err
		}
		blockedBy := loadgate.WhyWriteback
		if calm {
			blockedBy, err = why(ctx)
			if err != nil {
				return false, waits, err
			}
			if blockedBy == "" {
				return true, waits, nil
			}
		}
		if !time.Now().Before(deadline) {
			charge(blockedBy)
			return false, waits, nil
		}
		timer := time.NewTimer(min(poll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			charge(blockedBy)
			return false, waits, ctx.Err()
		case <-timer.C:
			charge(blockedBy)
		}
	}
}

func waitTotal(waits loadgate.Waits) time.Duration {
	var total time.Duration
	for _, d := range waits {
		total += d
	}
	return total
}

func formatTrimWaits(waits loadgate.Waits) string {
	order := []string{loadgate.WhyIO, loadgate.WhyWriteback, loadgate.WhySwap, loadgate.WhyQuiet}
	names := map[string]string{
		loadgate.WhyIO: "disk", loadgate.WhyWriteback: "write-out",
		loadgate.WhySwap: "memory", loadgate.WhyQuiet: "quiet",
	}
	known := map[string]bool{}
	parts := make([]string, 0, len(waits))
	appendWait := func(why string) {
		if d := waits[why].Round(time.Second); d > 0 {
			name := names[why]
			if name == "" {
				name = why
			}
			parts = append(parts, fmt.Sprintf("%s %s", name, d))
		}
	}
	for _, why := range order {
		known[why] = true
		appendWait(why)
	}
	var other []string
	for why := range waits {
		if !known[why] {
			other = append(other, why)
		}
	}
	sort.Strings(other)
	for _, why := range other {
		appendWait(why)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
