package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/checks"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/leaks"
	"github.com/aiseeq/graft/internal/userconfig"
)

// leakSettings returns the user's leak settings; ok is false when the user
// has none.
func leakSettings() (*userconfig.Leaks, bool, error) {
	ucfg, _, err := userconfig.Load()
	if errors.Is(err, userconfig.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if ucfg.Leaks == nil {
		return nil, false, nil
	}
	return ucfg.Leaks, true, nil
}

func loadLeakRules(l *userconfig.Leaks, log io.Writer) (*leaks.Rules, error) {
	allow := map[string]string{}
	for _, a := range l.Allow {
		allow[a.Term] = a.Reason
	}
	rules, err := leaks.Load(leaks.Options{Sources: l.PrivateSources, TermsFile: l.TermsFile, MinNameLength: l.MinNameLength, Allow: allow})
	if err != nil {
		return nil, fmt.Errorf("leak check (user config leaks): %w", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("leak check: locating the cache: %w", err)
	}
	corpus, err := leaks.LoadCorpus(filepath.Join(cacheDir, "graft", "leaks"), append(slices.Clone(l.PublicRemotes), rules.Modules()...), log)
	if err != nil {
		return nil, fmt.Errorf("leak check: %w", err)
	}
	rules.DropCommon(corpus)
	return rules, nil
}

// publicRemote returns the first remote of repo under a public prefix.
func publicRemote(repo *gitx.Repo, l *userconfig.Leaks) (string, bool, error) {
	remotes, err := repo.Remotes()
	if err != nil {
		return "", false, err
	}
	for _, r := range remotes {
		url, err := repo.Git("remote", "get-url", r)
		if err != nil {
			return "", false, err
		}
		if l.RemoteIsPublic(strings.TrimSpace(url)) {
			return r, true, nil
		}
	}
	return "", false, nil
}

// leakFindings scans the staged lines and the message of a commit to a public
// repository for the user's private names and terms.
func leakFindings(repo *gitx.Repo, message string, log io.Writer) ([]checks.Finding, error) {
	settings, ok, err := leakSettings()
	if err != nil || !ok {
		return nil, err
	}
	if _, public, err := publicRemote(repo, settings); err != nil || !public {
		return nil, err
	}
	rules, err := loadLeakRules(settings, log)
	if err != nil {
		return nil, err
	}
	lines, err := leaks.StagedLines(repo)
	if err != nil {
		return nil, err
	}
	base := ""
	hasHead, err := repo.HasHead()
	if err != nil {
		return nil, err
	}
	if hasHead {
		base = "HEAD"
	}
	found, err := rules.Scan(repo, base, lines, message)
	if err != nil {
		return nil, err
	}
	findings := make([]checks.Finding, 0, len(found))
	for _, f := range found {
		findings = append(findings, checks.Finding{Path: f.Path, Line: f.Line, Reason: f.Reason, Leak: true})
	}
	return findings, nil
}

// Leaks scans the commits of a revision range the way graft commit scans a
// public repository's changes: each commit's added lines and message, names
// the repository already had in the parent excluded. It reports and fails on
// findings, for auditing history.
func (a *App) Leaks(revRange string) error {
	repo, err := gitx.Open(a.Dir)
	if err != nil {
		return err
	}
	settings, ok, err := leakSettings()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no leaks section in the user config: nothing to look for")
	}
	rules, err := loadLeakRules(settings, a.Stderr)
	if err != nil {
		return err
	}
	out, err := repo.Git("rev-list", "--reverse", "--no-merges", revRange)
	if err != nil {
		return err
	}
	commits := strings.Fields(out)
	total := 0
	for _, c := range commits {
		lines, err := leaks.CommitLines(repo, c)
		if err != nil {
			return err
		}
		msg, err := repo.Git("log", "-1", "--format=%B", c)
		if err != nil {
			return err
		}
		base := ""
		if ok, err := repo.RevExists(c + "^"); err != nil {
			return err
		} else if ok {
			base = c + "^"
		}
		found, err := rules.Scan(repo, base, lines, strings.TrimRight(msg, "\n"))
		if err != nil {
			return err
		}
		if len(found) == 0 {
			continue
		}
		subject, _, _ := strings.Cut(msg, "\n")
		a.printf("%s %s", gitx.Short(c), subject)
		for _, f := range found {
			fmt.Fprintf(a.Stdout, "  %s:%d: %s\n", f.Path, f.Line, f.Reason)
		}
		total += len(found)
	}
	a.printf("%d commits scanned against %d private names", len(commits), rules.Names())
	if total > 0 {
		return fmt.Errorf("%d private names or terms found", total)
	}
	return nil
}
