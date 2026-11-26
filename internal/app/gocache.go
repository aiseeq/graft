package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/checks"
	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gocache"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/lock"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

const maxDirtyWritebackKB = 64 * 1024
const trimKind = "gocache-trim"

var freeSpace = availableFreeSpace
var trimSyncFS func(string) error
var trimWithOptions = gocache.TrimWithOptions

// GocacheTrim removes, with apply, the entries of the Go build cache of
// go env GOCACHE not used for longer than older, or tells how many it
// would; it needs no project.
func (a *App) GocacheTrim(ctx context.Context, older time.Duration, apply bool, maxWait time.Duration, freeBelow int64, batchSync string) (retErr error) {
	cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "go", "env", "GOCACHE")
	if err != nil {
		return err
	}
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
	freeBefore, skip, err := checkTrimFreeSpace(a, dir, freeBelow)
	if err != nil || skip {
		return err
	}
	trimLock, err := acquireGocacheTrimLock(ctx, dir, a.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := trimLock.Release(); releaseErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("releasing the gocache trim lock: %w", releaseErr))
		}
	}()
	if freeBelow > 0 {
		freeBefore, skip, err = checkTrimFreeSpace(a, dir, freeBelow)
		if err != nil || skip {
			return err
		}
	}
	options, waits, err := makeGocacheTrimOptions(ctx, a, apply, maxWait, batchSync)
	if err != nil {
		return err
	}
	r, err := trimWithOptions(dir, older, time.Now(), options)
	if err != nil {
		if apply && r.CacheValidated {
			var freeAfter *int64
			var freeAfterErr error
			if freeBelow > 0 {
				if measured, measureErr := freeSpace(dir); measureErr != nil {
					freeAfterErr = measureErr
				} else {
					freeAfter = &measured
				}
			}
			a.printf("%s", formatTrimSummary(dir, older, apply, options.BatchSync, r, waits, freeBelow > 0, freeBefore, freeAfter, freeAfterErr, err))
		}
		return err
	}
	var freeAfter *int64
	if freeBelow > 0 {
		measured, measureErr := freeSpace(dir)
		if measureErr != nil {
			return fmt.Errorf("measure free space on filesystem of %s after trim: %w", dir, measureErr)
		}
		freeAfter = &measured
	}
	a.printf("%s", formatTrimSummary(dir, older, apply, options.BatchSync, r, waits, freeBelow > 0, freeBefore, freeAfter, nil, nil))
	return nil
}

// gocacheTrimLockWait bounds the wait for another trim of the same cache only
// nominally: a timer or a person stops a stuck run long before, through ctx.
const gocacheTrimLockWait = 24 * time.Hour

// gocacheTrimLockPath sits beside the cache, so every trim of that cache
// meets the same lock whatever its environment; a symlinked path to the
// cache resolves to the same lock.
func gocacheTrimLockPath(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return filepath.Clean(dir) + ".graft-trim.lock"
}

func acquireGocacheTrimLock(ctx context.Context, dir string, log io.Writer) (*lock.Lock, error) {
	lockPath := gocacheTrimLockPath(dir)
	started := time.Now()
	trimLock, err := lock.Acquire(ctx, lockPath, gocacheTrimLockWait, "graft gocache trim", log)
	if err != nil {
		return nil, fmt.Errorf("gocache trim lock: %w", err)
	}
	if waited := time.Since(started); waited > 3*time.Second {
		fmt.Fprintf(log, "graft: gocache trim: waited %s for lock %s (%s)\n", waited.Round(time.Second), filepath.Base(lockPath), trimLock.WaitedBehind)
	}
	return trimLock, nil
}

func makeGocacheTrimOptions(ctx context.Context, a *App, apply bool, maxWait time.Duration, batchSync string) (gocache.Options, loadgate.Waits, error) {
	waits := loadgate.Waits{}
	remainingWait := maxWait
	options := gocache.Options{Apply: apply, BatchSync: batchSync, SyncFS: trimSyncFS}
	if !apply {
		return options, waits, nil
	}
	options.OnStart = func() { startTrimProbe(a.Stderr) }
	var reportedWait time.Duration
	options.OnBatch = func(number, entries int, bytes int64, started time.Time, unlink, syncTime time.Duration) {
		waited := waitTotal(waits) - reportedWait
		reportedWait = waitTotal(waits)
		syncDuration := "-"
		if options.BatchSync == "fs" {
			syncDuration = fmt.Sprintf("%dms", syncTime.Milliseconds())
		}
		fmt.Fprintf(a.Stderr, "graft: gocache trim: batch %d: started %s, %d entries, %s, unlink %dms, sync %s, syncfs %s, waited %dms\n",
			number, started.Format(time.RFC3339Nano), entries, checks.FormatSize(bytes), unlink.Milliseconds(), options.BatchSync, syncDuration, waited.Milliseconds())
	}
	load, err := userconfig.LoadSettings()
	if err != nil {
		return options, waits, fmt.Errorf("load machine pressure settings: %w", err)
	}
	checker := loadgate.NewDiskBusyChecker("gocache trim", trimKind, load, a.Stderr)
	options.BeforeBatch = func() (bool, error) {
		ok, batchWaits, err := waitForTrimBatch(ctx, remainingWait, checker.Poll, trimMemoryCalm, checker.Why)
		for why, d := range batchWaits {
			waits[why] += d
		}
		remainingWait = max(0, remainingWait-waitTotal(batchWaits))
		return ok, err
	}
	return options, waits, nil
}

func checkTrimFreeSpace(a *App, dir string, freeBelow int64) (int64, bool, error) {
	if freeBelow <= 0 {
		return 0, false, nil
	}
	free, err := freeSpace(dir)
	if err != nil {
		return 0, false, fmt.Errorf("measure free space on filesystem of %s: %w", dir, err)
	}
	if free >= freeBelow {
		a.printf("%s free on %s, at or above --free-below %s: nothing to do",
			checks.FormatSize(free), dir, checks.FormatSize(freeBelow))
		return free, true, nil
	}
	return free, false, nil
}

func formatTrimSummary(dir string, older time.Duration, apply bool, batchSync string, r gocache.Result, waits loadgate.Waits, measureFree bool, freeBefore int64, freeAfter *int64, freeAfterErr, interrupted error) string {
	verb := "removed"
	if !apply {
		verb = "dry run: graft gocache trim --apply removes"
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "%s %d entries, %s, unused for %s in %s", verb, r.Removed,
		checks.FormatSize(r.RemovedBytes), older, dir)
	if apply {
		if interrupted != nil {
			countWord, remainWord := "entries", "remain"
			if r.Remaining == 1 {
				countWord, remainWord = "entry", "remains"
			}
			fmt.Fprintf(&summary, "; at least %d %s %s (%s; walk stopped)", r.Remaining, countWord, remainWord, checks.FormatSize(r.RemainingBytes))
		} else {
			fmt.Fprintf(&summary, "; %d entries, %s remain", r.Remaining, checks.FormatSize(r.RemainingBytes))
		}
	}
	fmt.Fprintf(&summary, "; %d entries, %s kept", r.Kept, checks.FormatSize(r.KeptBytes))
	if measureFree {
		fmt.Fprintf(&summary, "; free space %s before", checks.FormatSize(freeBefore))
		if freeAfter != nil {
			fmt.Fprintf(&summary, ", %s after", checks.FormatSize(*freeAfter))
		} else if freeAfterErr != nil {
			fmt.Fprintf(&summary, ", after unavailable: %v", freeAfterErr)
		}
	}
	if apply {
		fmt.Fprintf(&summary, "; waited %s%s; sync %s", waitTotal(waits).Round(time.Second), formatTrimWaits(waits), batchSync)
	}
	if interrupted != nil {
		fmt.Fprintf(&summary, "; interrupted: %v", interrupted)
	}
	return summary.String()
}

func trimMemoryCalm() (bool, error) {
	dirtyWriteback, err := loadgate.DirtyWritebackKB()
	if err != nil {
		return false, err
	}
	return dirtyWriteback < maxDirtyWritebackKB, nil
}

func waitForTrimBatch(ctx context.Context, maxWait time.Duration, poll func() time.Duration, dirty func() (bool, error), why func(context.Context) (string, error)) (bool, loadgate.Waits, error) {
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
		timer := time.NewTimer(min(poll(), time.Until(deadline)))
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
