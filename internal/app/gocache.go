package app

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/checks"
	"github.com/aiseeq/graft/v2/internal/gocache"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/userconfig"
)

const maxDirtyWritebackKB = 64 * 1024
const trimKind = "gocache-trim"

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
	var waited time.Duration
	remainingWait := maxWait
	var options gocache.Options
	options.Apply = apply
	if apply {
		load, err := userconfig.LoadSettings()
		if err != nil {
			return fmt.Errorf("load machine pressure settings: %w", err)
		}
		checker := loadgate.NewBusyChecker("gocache trim", trimKind, load, a.Stderr)
		options.BeforeBatch = func() (bool, error) {
			ok, d, err := waitForTrimBatch(ctx, remainingWait, load.Poll,
				trimMemoryCalm,
				checker.Busy)
			waited += d
			remainingWait = max(0, remainingWait-d)
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
		a.printf("%s %d entries, %s, unused for %s in %s; %d entries, %s remain; %d entries, %s kept; waited %s",
			verb, r.Removed, checks.FormatSize(r.RemovedBytes), older, dir, r.Remaining, checks.FormatSize(r.RemainingBytes),
			r.Kept, checks.FormatSize(r.KeptBytes), waited.Round(time.Second))
	} else {
		a.printf("%s %d entries, %s, unused for %s in %s; %d entries, %s kept",
			verb, r.Removed, checks.FormatSize(r.RemovedBytes), older, dir, r.Kept, checks.FormatSize(r.KeptBytes))
	}
	return nil
}

func trimMemoryCalm() (bool, error) {
	dirtyWriteback, err := loadgate.DirtyWritebackKB()
	if err != nil {
		return false, err
	}
	return dirtyWriteback < maxDirtyWritebackKB, nil
}

func waitForTrimBatch(ctx context.Context, maxWait, poll time.Duration, dirty func() (bool, error), busy func(context.Context) (bool, error)) (bool, time.Duration, error) {
	start := time.Now()
	deadline := start.Add(maxWait)
	for {
		if err := ctx.Err(); err != nil {
			return false, time.Since(start), err
		}
		calm, err := dirty()
		if err != nil {
			return false, time.Since(start), err
		}
		if calm {
			var isBusy bool
			isBusy, err = busy(ctx)
			if err != nil {
				return false, time.Since(start), err
			}
			if !isBusy {
				return true, time.Since(start), nil
			}
		}
		if !time.Now().Before(deadline) {
			return false, time.Since(start), nil
		}
		timer := time.NewTimer(min(poll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, time.Since(start), ctx.Err()
		case <-timer.C:
		}
	}
}
