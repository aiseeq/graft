package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// flagsEvery is how often commit and deploy read the flag of an environment
// on their own: the daily look graft flags is meant for, taken for whoever
// forgot it.
const flagsEvery = 24 * time.Hour

// flagsTimeout bounds that read: it is a reminder, not part of the work
// committed or deployed.
const flagsTimeout = 15 * time.Second

// dailyFlags prints the flag of env (empty means flags.default_env) when no
// graft has looked at it for a day. Commit and deploy have done their work
// by then, so nothing here fails them: a journal that cannot be read is a
// warning, and the next try is a day later all the same, so that a commit
// without the network does not wait for it every time.
func (a *App) dailyFlags(ctx context.Context, repo *gitx.Repo, cfg *config.Config, env string) {
	f := cfg.Flags
	if f == nil {
		return
	}
	if env == "" {
		env = f.DefaultEnv
	}
	if !f.Reads(env) {
		return
	}
	stamp, err := flagsStamp(repo, env)
	if err != nil {
		a.warn("flags (%s) not checked: %v", env, err)
		return
	}
	fi, err := os.Stat(stamp)
	switch {
	case err == nil && time.Since(fi.ModTime()) < flagsEvery:
		return
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		a.warn("flags (%s) not checked: %v", env, err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, flagsTimeout)
	defer cancel()
	readErr := a.printFlags(ctx, repo, cfg, env)
	if err := touch(stamp); err != nil {
		a.warn("flags (%s): %v", env, err)
	}
	if readErr != nil {
		a.warn("flags (%s) not read: %v; graft flags --env %s reads them", env, readErr, env)
	}
}

func (a *App) printFlags(ctx context.Context, repo *gitx.Repo, cfg *config.Config, env string) error {
	j, err := a.journal(repo, cfg, env)
	if err != nil {
		return err
	}
	c, err := j.Count(ctx)
	if err != nil {
		return err
	}
	if c.Raised() {
		a.printf("%s: graft flags --env %s", c.Headline(env), env)
	} else {
		a.printf("%s", c.Headline(env))
	}
	return nil
}

// flagsStamp is the file whose time says when the flag of env was last read
// for the repository, shared by its worktrees.
func flagsStamp(repo *gitx.Repo, env string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache: %w", err)
	}
	sum := sha256.Sum256([]byte(repo.CommonDir + "\x00" + env))
	return filepath.Join(cache, "graft", "flags", hex.EncodeToString(sum[:12])), nil
}

// markFlagsRead records that the flag of env was just read.
func markFlagsRead(repo *gitx.Repo, env string) error {
	stamp, err := flagsStamp(repo, env)
	if err != nil {
		return err
	}
	return touch(stamp)
}

func touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}
