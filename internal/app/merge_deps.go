package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// runMergeDeps runs the configured publication checks in the given tree.
// Merge and a direct trunk commit use the same task runner and report.
func runMergeDeps(a *App, ctx context.Context, repo *gitx.Repo, cfg *config.Config, skip bool, reason string) error {
	if skip {
		a.printf("merge deps skipped: %s", strings.TrimSpace(reason))
		return nil
	}
	if len(cfg.Merge.Deps) == 0 {
		a.printf("merge deps: skipped (none configured)")
		return nil
	}
	a.printf("merge deps: %s", strings.Join(cfg.Merge.Deps, ", "))
	if err := a.runner(repo, cfg).RunDeps(ctx, cfg.Merge.Deps); err != nil {
		return fmt.Errorf("merge deps failed in %s: %w", repo.Root, err)
	}
	return nil
}
