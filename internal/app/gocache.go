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
)

// GocacheTrim removes, with apply, the entries of the Go build cache of
// go env GOCACHE not used for longer than older, or tells how many it
// would; it needs no project.
func (a *App) GocacheTrim(ctx context.Context, older time.Duration, apply bool) error {
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
	r, err := gocache.Trim(dir, older, time.Now(), apply)
	if err != nil {
		return err
	}
	verb := "removed"
	if !apply {
		verb = "dry run: graft gocache trim --apply removes"
	}
	a.printf("%s %d entries, %s, unused for %s in %s; %d entries, %s kept",
		verb, r.Removed, checks.FormatSize(r.RemovedBytes), older, dir, r.Kept, checks.FormatSize(r.KeptBytes))
	return nil
}
