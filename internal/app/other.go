package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/hooks"
	"github.com/aiseeq/graft/internal/version"
)

// Amend folds the work tree into the last commit through the same gate and
// checks as a new commit, keeping its message. It does not push: the commit it
// rewrites must not be published yet.
func (a *App) Amend(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.withLock(ctx, repo, cfg, "graft amend", func() error {
		if err := checkAmendable(repo); err != nil {
			return err
		}
		if err := a.gate(ctx, repo, cfg); err != nil {
			return err
		}
		if _, err := repo.Git("add", "-A"); err != nil {
			return err
		}
		if err := runChecks(repo, cfg, "", a.Stderr); err != nil {
			return err
		}
		if _, err := repo.GitEnv(markerEnv, "commit", "--amend", "--no-edit"); err != nil {
			return err
		}
		head, err := repo.Git("rev-parse", "--short", "HEAD")
		if err != nil {
			return err
		}
		a.printf("amended the last commit, now %s (not pushed)", strings.TrimSpace(head))
		return nil
	})
}

// checkAmendable refuses to amend a published commit, a missing one, or with
// nothing to fold in.
func checkAmendable(repo *gitx.Repo) error {
	if err := requireNormalContext(repo); err != nil {
		return err
	}
	hasHead, err := repo.HasHead()
	if err != nil {
		return err
	}
	if !hasHead {
		return errors.New("there is no commit to amend yet: use graft commit")
	}
	published, err := repo.RemotesContaining("HEAD")
	if err != nil {
		return err
	}
	if len(published) > 0 {
		return fmt.Errorf("the last commit is already on %s: amending it would rewrite published history; make a new commit instead: graft commit -m \"fix: ...\"", strings.Join(published, ", "))
	}
	clean, err := repo.IsClean()
	if err != nil {
		return err
	}
	if clean {
		return errors.New("nothing to amend: the work tree is clean")
	}
	return nil
}

// ReleaseOptions are the arguments of graft release.
type ReleaseOptions struct {
	Level version.Level
	// Version, when set, is the exact version to release (git-tag mode).
	Version string
}

// InitialRelease is the version graft release tags in git-tag mode when the
// history has no version tag yet.
var InitialRelease = version.Semver{Major: 0, Minor: 1, Patch: 0}

// Release tags the pushed HEAD with the project version and pushes the tag.
// In file mode the version is the committed version file; in git-tag mode it
// is the next version after the latest tag.
func (a *App) Release(ctx context.Context, opts ReleaseOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	return a.withLock(ctx, repo, cfg, "graft release", func() error {
		next, err := releaseVersion(repo, cfg, opts)
		if err != nil {
			return err
		}
		tag := cfg.Version.TagPrefix + next.String()
		exists, err := tagExists(repo, tag)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("tag %s already exists", tag)
		}
		remotes, err := pushRemotes(repo, cfg)
		if err != nil {
			return err
		}
		if err := requirePublished(repo, remotes); err != nil {
			return err
		}
		if _, err := repo.Git("tag", "-a", tag, "-m", tag); err != nil {
			return err
		}
		a.printf("tagged %s", tag)
		return a.push(repo, remotes, []string{"refs/tags/" + tag})
	})
}

// requirePublished refuses to tag a commit the remotes do not have yet.
func requirePublished(repo *gitx.Repo, remotes []string) error {
	published, err := repo.RemotesContaining("HEAD")
	if err != nil {
		return err
	}
	var missing []string
	for _, r := range remotes {
		if !slices.Contains(published, r) {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the commit to tag is not on %s yet: push it first (graft commit pushes it), then release", strings.Join(missing, ", "))
	}
	return nil
}

func releaseVersion(repo *gitx.Repo, cfg *config.Config, opts ReleaseOptions) (version.Semver, error) {
	switch cfg.Version.Mode {
	case config.ModeFile:
		if opts.Level != version.Patch || opts.Version != "" {
			return version.Semver{}, errors.New("in version.mode file the version is bumped by graft commit; release takes no --minor, --major or --version")
		}
		content, err := repo.ShowFile("HEAD", cfg.Version.File)
		if err != nil {
			return version.Semver{}, err
		}
		raw, err := version.Targets(cfg.Version)[0].Extract(content)
		if err != nil {
			return version.Semver{}, err
		}
		return version.Parse(raw)
	case config.ModeGitTag:
		latest, found, err := latestTag(repo, cfg.Version.TagPrefix)
		if err != nil {
			return version.Semver{}, err
		}
		if opts.Version != "" {
			if opts.Level != version.Patch {
				return version.Semver{}, errors.New("--version and --minor/--major exclude each other")
			}
			want, err := version.Parse(opts.Version)
			if err != nil {
				return version.Semver{}, err
			}
			if found && !latest.Less(want) {
				return version.Semver{}, fmt.Errorf("--version %s is not above the latest tag %s%s", want, cfg.Version.TagPrefix, latest)
			}
			return want, nil
		}
		if !found {
			return InitialRelease, nil
		}
		return latest.Bump(opts.Level), nil
	default:
		return version.Semver{}, fmt.Errorf("version.mode %s has no releases", cfg.Version.Mode)
	}
}

// latestTag finds the highest MAJOR.MINOR.PATCH tag reachable from HEAD.
// Tags that are not plain versions (v1.0.0-rc1, other prefixes) are ignored:
// graft never creates them.
func latestTag(repo *gitx.Repo, prefix string) (version.Semver, bool, error) {
	hasHead, err := repo.HasHead()
	if err != nil || !hasHead {
		return version.Semver{}, false, err
	}
	out, err := repo.Git("tag", "--list", "--merged", "HEAD", prefix+"*")
	if err != nil {
		return version.Semver{}, false, err
	}
	var (
		best  version.Semver
		found bool
	)
	for _, tag := range strings.Fields(out) {
		raw := strings.TrimPrefix(tag, prefix)
		if !version.IsPlain(raw) {
			continue
		}
		v, err := version.Parse(raw)
		if err != nil {
			return version.Semver{}, false, fmt.Errorf("tag %s: %w", tag, err)
		}
		if !found || best.Less(v) {
			best, found = v, true
		}
	}
	return best, found, nil
}

// Version prints the project version.
func (a *App) Version(describe bool) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if describe {
		v, err := describeVersion(repo, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintln(a.Stdout, v)
		return nil
	}
	v, err := projectVersion(repo, cfg)
	if errors.Is(err, errNoVersionTags) {
		head, headErr := repo.Git("rev-parse", "--short", "HEAD")
		if headErr != nil {
			return fmt.Errorf("no version tags and no commits yet: %w", headErr)
		}
		fmt.Fprintf(a.Stdout, "no version tags yet (HEAD %s)\n", strings.TrimSpace(head))
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, v)
	return nil
}

var (
	errNoVersionTags = errors.New("no version tags yet")
	errNoCommits     = errors.New("the repository has no commits yet")
)

// describeVersion identifies the build of the work tree for stamping into
// binaries: the version file, or git describe from the highest version tag,
// with -dirty for uncommitted changes to tracked files. Without a version
// (mode none, or no tag yet) it reads like a describe of a v0.0.0 tag at the
// root: v0.0.0-<commits>-g<hash>.
func describeVersion(repo *gitx.Repo, cfg *config.Config) (string, error) {
	hasHead, err := repo.HasHead()
	if err != nil {
		return "", err
	}
	if !hasHead {
		return "", errNoCommits
	}
	dirty, err := trackedChanges(repo)
	if err != nil {
		return "", err
	}
	suffix := ""
	if dirty {
		suffix = "-dirty"
	}
	v, err := projectVersion(repo, cfg)
	switch {
	case err == nil && cfg.Version.Mode == config.ModeFile:
		return v + suffix, nil
	case err == nil:
		return v, nil // git describe --dirty has added the suffix
	case !errors.Is(err, errNoVersionTags) && cfg.Version.Mode != config.ModeNone:
		return "", err
	}
	count, err := repo.Git("rev-list", "--count", "HEAD")
	if err != nil {
		return "", err
	}
	short, err := repo.Git("rev-parse", "--short=7", "HEAD")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s0.0.0-%s-g%s%s", cfg.Version.TagPrefix, strings.TrimSpace(count), strings.TrimSpace(short), suffix), nil
}

// trackedChanges reports uncommitted changes to tracked files, the way git
// describe --dirty sees them.
func trackedChanges(repo *gitx.Repo) (bool, error) {
	out, err := repo.Git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// projectVersion is the version of the work tree: the version file, or git
// describe from the highest version tag.
func projectVersion(repo *gitx.Repo, cfg *config.Config) (string, error) {
	switch cfg.Version.Mode {
	case config.ModeFile:
		v, err := version.Current(repo.Root, cfg.Version)
		if err != nil {
			return "", err
		}
		return v.String(), nil
	case config.ModeGitTag:
		latest, found, err := latestTag(repo, cfg.Version.TagPrefix)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errNoVersionTags
		}
		// Describe from the highest version tag by name: with several tags on
		// one commit, git describe alone may pick a lower one.
		out, err := repo.Git("describe", "--tags", "--match", cfg.Version.TagPrefix+latest.String(), "--dirty")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	default:
		return "", fmt.Errorf("version.mode is %s: the project has no version", cfg.Version.Mode)
	}
}

// Init installs the hooks and the pinned tools that fail their check.
func (a *App) Init(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	changes, err := hooks.Install(repo, cfg.Hooks.Dir, cfg.MinGraft)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		a.printf("hooks already installed in %s, nothing changed", cfg.Hooks.Dir)
	}
	for _, c := range changes {
		a.printf("%s", c.What)
	}
	if err := a.initDotEnv(repo, cfg); err != nil {
		return err
	}
	if len(cfg.Tools) == 0 {
		return nil
	}
	return a.checkTools(ctx, cfg, true)
}

// initDotEnv copies dotenv_template to the dotenv file, readable by the owner
// only, when the dotenv file does not exist; an existing one is never touched.
func (a *App) initDotEnv(repo *gitx.Repo, cfg *config.Config) error {
	if cfg.DotEnvTemplate == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(cfg.DotEnvTemplate)))
	if err != nil {
		return fmt.Errorf("dotenv_template: %w", err)
	}
	path := filepath.Join(repo.Root, filepath.FromSlash(cfg.DotEnv))
	// O_EXCL: a file that appears meanwhile is not overwritten either.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		a.printf("%s exists, %s not copied", cfg.DotEnv, cfg.DotEnvTemplate)
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating %s: %w", cfg.DotEnv, err)
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err != nil {
		// A partial file would keep the next init from copying again.
		return errors.Join(fmt.Errorf("writing %s: %w", cfg.DotEnv, err), os.Remove(path))
	}
	a.printf("created %s from %s: fill in its values", cfg.DotEnv, cfg.DotEnvTemplate)
	return nil
}

// Check runs the staged content checks on demand.
func (a *App) Check() error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if err := runChecks(repo, cfg, "", a.Stderr); err != nil {
		return err
	}
	a.printf("checks passed")
	return nil
}

// Gate runs the gate on demand.
func (a *App) Gate(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if err := a.gate(ctx, repo, cfg); err != nil {
		return err
	}
	a.printf("gate passed")
	return nil
}
