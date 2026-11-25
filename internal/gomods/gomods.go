// Package gomods raises configured module requirements to their latest
// published versions.
package gomods

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/envname"
	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// latestTimeout bounds the lookup of the latest versions: a proxy that does
// not answer must not hold a commit.
const latestTimeout = time.Minute

// Raise raises the requirements own picks, in every go.mod of the work tree
// but under testdata and vendor, to their latest versions, then tidies the
// module. A latest version go cannot learn (offline) is a warning, the
// go.mod stays as it is; a failing go get or tidy is an error.
func Raise(ctx context.Context, repo *gitx.Repo, own func(path string) bool, log io.Writer, warning func(string, ...any)) error {
	return raise(ctx, repo, own, log, warning)
}

// RaiseForeign raises requirements other than the module itself and replaced
// modules. It runs separately so failures can roll back only this step.
func RaiseForeign(ctx context.Context, repo *gitx.Repo, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	return raiseForeign(ctx, repo, own, log, warning)
}

func raise(ctx context.Context, repo *gitx.Repo, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	goPath, err := findGo()
	if err != nil || goPath == "" {
		return err
	}
	files, err := repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("listing tracked files: %w", err)
	}
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return fmt.Errorf("listing git environment variables: %w", err)
	}
	env := envWith(gitx.Without(os.Environ(), drop), "GOWORK", "off")
	for name := range strings.SplitSeq(files, "\x00") {
		if filepath.Base(name) != "go.mod" || excluded(name) {
			continue
		}
		path := filepath.Join(repo.Root, filepath.FromSlash(name))
		if own == nil {
			return fmt.Errorf("own module predicate is required")
		}
		if err := raiseFile(ctx, path, name, goPath, env, own, log, warning); err != nil {
			return err
		}
	}
	return nil
}

// raiseForeign visits the same module files as raise but selects only
// requirements outside the configured own-module prefixes.
func raiseForeign(ctx context.Context, repo *gitx.Repo, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	goPath, err := findGo()
	if err != nil || goPath == "" {
		return err
	}
	files, err := repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("listing tracked files: %w", err)
	}
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return fmt.Errorf("listing git environment variables: %w", err)
	}
	env := envWith(gitx.Without(os.Environ(), drop), "GOWORK", "off")
	for name := range strings.SplitSeq(files, "\x00") {
		if filepath.Base(name) != "go.mod" || excluded(name) {
			continue
		}
		path := filepath.Join(repo.Root, filepath.FromSlash(name))
		if err := raiseForeignFile(ctx, path, name, goPath, env, own, log, warning); err != nil {
			return err
		}
	}
	return nil
}

func findGo() (string, error) {
	goPath, err := exec.LookPath("go")
	if errors.Is(err, exec.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("looking up go: %w", err)
	}
	return goPath, nil
}

func excluded(name string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(name), "/") {
		if part == "testdata" || part == "vendor" {
			return true
		}
	}
	return false
}

type candidate struct{ path, version string }

// selectCandidates is the requirements of a go.mod own picks, but the
// module itself and a module the go.mod replaces.
func selectCandidates(data []byte, own func(path string) bool) ([]candidate, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parsing go.mod: %w", err)
	}
	replaced := make(map[string]bool, len(f.Replace))
	for _, r := range f.Replace {
		replaced[r.Old.Path] = true
	}
	var out []candidate
	for _, r := range f.Require {
		if r.Mod.Path == f.Module.Mod.Path || replaced[r.Mod.Path] || !own(r.Mod.Path) {
			continue
		}
		out = append(out, candidate{r.Mod.Path, r.Mod.Version})
	}
	return out, nil
}

func raiseFile(ctx context.Context, path, display, goPath string, env []string, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", display, err)
	}
	candidates, err := selectCandidates(data, own)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if len(candidates) == 0 {
		return nil
	}
	latest, err := latestVersions(ctx, filepath.Dir(path), goPath, env, candidates)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	for _, c := range candidates {
		if m := latest[c.path]; m.Version == "" {
			if m.Err == "" {
				m.Err = "go list -m gave no version"
			}
			reason, _, _ := strings.Cut(strings.TrimSpace(m.Err), "\n")
			warning("%s: %s: the latest version is unknown: %s", display, c.path, reason)
		}
	}
	var upgraded []candidate
	args := []string{"get"}
	for _, c := range candidates {
		v := latest[c.path].Version
		if v != "" && semver.Compare(v, c.version) > 0 {
			upgraded = append(upgraded, candidate{c.path, c.version})
			args = append(args, c.path+"@"+v)
		}
	}
	if len(upgraded) == 0 {
		return nil
	}
	out, stderr, err := execx.Run(ctx, filepath.Dir(path), goPath, env, args...)
	if err != nil {
		return goFailure(display, "go get", stderr, out, err)
	}
	fmt.Fprintf(log, "graft: %s: raised modules: %s\n", display, formatRaisedModules(upgraded, latest))
	out, stderr, err = execx.Run(ctx, filepath.Dir(path), goPath, env, "mod", "tidy")
	if err != nil {
		return goFailure(display, "go mod tidy", stderr, out, err)
	}
	_, statErr := os.Stat(filepath.Join(filepath.Dir(path), "vendor", "modules.txt"))
	if statErr == nil {
		out, stderr, err = execx.Run(ctx, filepath.Dir(path), goPath, env, "mod", "vendor")
		if err != nil {
			return goFailure(display, "go mod vendor", stderr, out, err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking %s vendor/modules.txt: %w", display, statErr)
	}
	return nil
}

func raiseForeignFile(ctx context.Context, path, display, goPath string, env []string, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", display, err)
	}
	candidates, err := selectCandidates(data, func(path string) bool { return !own(path) })
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if len(candidates) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "vendor", "modules.txt")); err == nil {
		warning("%s: foreign Go module raises skipped because vendor/modules.txt is present", display)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking %s vendor/modules.txt: %w", display, err)
	}
	snapshot, err := snapshotForeignFiles(path, display, data)
	if err != nil {
		return err
	}
	latest, err := latestVersions(ctx, filepath.Dir(path), goPath, env, candidates)
	if err != nil {
		if restoreErr := restoreForeignFiles(path, display, snapshot); restoreErr != nil {
			return restoreErr
		}
		return fmt.Errorf("%s: %w", display, err)
	}
	upgraded, args := foreignUpgradeArgs(candidates, latest, display, warning)
	if len(upgraded) == 0 {
		if err := restoreForeignFiles(path, display, snapshot); err != nil {
			return err
		}
		return nil
	}
	if err := applyForeignUpgrades(ctx, path, display, goPath, env, args, upgraded, latest, snapshot, log, warning); err != nil {
		return err
	}
	return nil
}

func foreignUpgradeArgs(candidates []candidate, latest map[string]LatestModule, display string, warning func(string, ...any)) ([]candidate, []string) {
	var upgraded []candidate
	args := []string{"get"}
	for _, c := range candidates {
		m := latest[c.path]
		if m.Version == "" {
			reason := m.Err
			if reason == "" {
				reason = "go list -m gave no version"
			}
			warning("%s: %s: foreign module latest version is unknown: %s", display, c.path, lastOutputLine(reason))
			continue
		}
		if !foreignUpgrade(c.version, m.Version) {
			continue
		}
		upgraded = append(upgraded, c)
		args = append(args, c.path+"@"+m.Version)
	}
	return upgraded, args
}

type foreignSnapshot struct {
	goMod, goSum           []byte
	goSumExists            bool
	goDirective, toolchain string
}

func snapshotForeignFiles(path, display string, data []byte) (foreignSnapshot, error) {
	sumPath := filepath.Join(filepath.Dir(path), "go.sum")
	sumData, sumErr := os.ReadFile(sumPath)
	sumExists := sumErr == nil
	if sumErr != nil && !errors.Is(sumErr, os.ErrNotExist) {
		return foreignSnapshot{}, fmt.Errorf("reading %s go.sum: %w", display, sumErr)
	}
	goDirective, toolchain, err := goDirectives(data)
	if err != nil {
		return foreignSnapshot{}, fmt.Errorf("%s: %w", display, err)
	}
	return foreignSnapshot{goMod: data, goSum: sumData, goSumExists: sumExists, goDirective: goDirective, toolchain: toolchain}, nil
}

func applyForeignUpgrades(ctx context.Context, path, display, goPath string, env, args []string, upgraded []candidate, latest map[string]LatestModule, snapshot foreignSnapshot, log io.Writer, warning func(string, ...any)) error {
	rollback := func() error {
		return restoreForeignFiles(path, display, snapshot)
	}
	out, stderr, runErr := execx.Run(ctx, filepath.Dir(path), goPath, env, args...)
	if runErr == nil {
		out, stderr, runErr = execx.Run(ctx, filepath.Dir(path), goPath, env, "mod", "tidy")
	}
	if runErr != nil {
		reason := lastOutputLine(joinOutput(stderr, out, runErr))
		if err := rollback(); err != nil {
			return err
		}
		warning("%s: foreign modules %s were rolled back: %s", display, candidateNames(upgraded), reason)
		return nil
	}
	changed, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading raised %s: %w", display, err)
	}
	newGo, newToolchain, err := goDirectives(changed)
	if err != nil {
		return fmt.Errorf("%s after foreign raise: %w", display, err)
	}
	if newGo != snapshot.goDirective || newToolchain != snapshot.toolchain {
		if err := rollback(); err != nil {
			return err
		}
		warning("%s: foreign modules %s were rolled back because go/toolchain directive changed", display, candidateNames(upgraded))
		return nil
	}
	fmt.Fprintf(log, "graft: %s: raised foreign modules: %s\n", display, formatRaisedModules(upgraded, latest))
	return nil
}

func restoreForeignFiles(path, display string, snapshot foreignSnapshot) error {
	sumPath := filepath.Join(filepath.Dir(path), "go.sum")
	currentMod, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s before restore: %w", display, err)
	}
	if errors.Is(err, os.ErrNotExist) || !bytes.Equal(currentMod, snapshot.goMod) {
		if err := os.WriteFile(path, snapshot.goMod, 0o666); err != nil {
			return fmt.Errorf("restoring %s: %w", display, err)
		}
	}
	if snapshot.goSumExists {
		currentSum, err := os.ReadFile(sumPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading %s go.sum before restore: %w", display, err)
		}
		if errors.Is(err, os.ErrNotExist) || !bytes.Equal(currentSum, snapshot.goSum) {
			if err := os.WriteFile(sumPath, snapshot.goSum, 0o666); err != nil {
				return fmt.Errorf("restoring %s go.sum: %w", display, err)
			}
		}
	} else {
		_, err := os.Stat(sumPath)
		if err == nil {
			if err := os.Remove(sumPath); err != nil {
				return fmt.Errorf("removing raised %s go.sum: %w", display, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking raised %s go.sum: %w", display, err)
		}
	}
	return nil
}

func foreignUpgrade(current, latest string) bool {
	return semver.IsValid(current) && semver.IsValid(latest) &&
		semver.Compare(latest, current) > 0 &&
		semver.Major(latest) == semver.Major(current) &&
		!strings.Contains(latest, "+incompatible") &&
		(module.IsPseudoVersion(current) || !module.IsPseudoVersion(latest))
}

func goDirectives(data []byte) (string, string, error) {
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return "", "", fmt.Errorf("parsing go.mod directives: %w", err)
	}
	toolchain := ""
	if f.Toolchain != nil {
		toolchain = f.Toolchain.Name
	}
	goVersion := ""
	if f.Go != nil {
		goVersion = f.Go.Version
	}
	return goVersion, toolchain, nil
}

func lastOutputLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return "unknown error"
	}
	return lines[len(lines)-1]
}

func candidateNames(candidates []candidate) string {
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = c.path
	}
	return strings.Join(names, ", ")
}

func formatRaisedModules(upgraded []candidate, latest map[string]LatestModule) string {
	changes := make([]string, 0, len(upgraded))
	for _, c := range upgraded {
		changes = append(changes, fmt.Sprintf("%s %s -> %s", c.path, c.version, latest[c.path].Version))
	}
	return strings.Join(changes, ", ")
}

// LatestModule is what go list -m -e says of a module@latest query.
type LatestModule struct {
	Version string
	Err     string
}

// LatestVersions asks go for the latest version of each module in one query.
// A module go cannot resolve has Err set; command and decoding failures are
// returned to the caller.
func LatestVersions(ctx context.Context, dir string, modules []string) (map[string]LatestModule, error) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("looking up go: %w", err)
	}
	candidates := make([]candidate, len(modules))
	for i, module := range modules {
		candidates[i] = candidate{path: module}
	}
	latest, err := latestVersions(ctx, dir, goPath, envWith(os.Environ(), "GOWORK", "off"), candidates)
	if err != nil {
		return nil, err
	}
	return latest, nil
}

// latestVersions asks go for the latest version of every candidate in one
// go list, with -e so that a module go cannot look up leaves its error
// instead of failing the others. A go list that fails outright (offline
// under GOFLAGS=-mod=mod, a timeout) gives every candidate its error.
func latestVersions(ctx context.Context, dir, goPath string, env []string, candidates []candidate) (map[string]LatestModule, error) {
	ctx, cancel := context.WithTimeout(ctx, latestTimeout)
	defer cancel()
	args := []string{"list", "-m", "-e", "-json"}
	for _, c := range candidates {
		args = append(args, c.path+"@latest")
	}
	out, stderr, err := execx.Run(ctx, dir, goPath, env, args...)
	latest := make(map[string]LatestModule, len(candidates))
	if err != nil {
		msg := joinOutput(stderr, out, err)
		for _, c := range candidates {
			latest[c.path] = LatestModule{Err: msg}
		}
		return latest, nil
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var m struct {
			Path, Version string
			Error         *struct{ Err string }
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		lm := LatestModule{Version: m.Version}
		if m.Error != nil {
			lm = LatestModule{Err: m.Error.Err}
		}
		latest[m.Path] = lm
	}
	return latest, nil
}

func joinOutput(stderr, stdout string, err error) string {
	var output []string
	if s := strings.TrimSpace(stderr); s != "" {
		output = append(output, s)
	}
	if s := strings.TrimSpace(stdout); s != "" {
		output = append(output, s)
	}
	if len(output) == 0 {
		return err.Error()
	}
	return strings.Join(output, "\n")
}

func goFailure(display, command, stderr, stdout string, err error) error {
	return fmt.Errorf("%s: %s failed: %w\n%s", display, command, err, strings.TrimSpace(joinOutput(stderr, stdout, err)))
}

func envWith(env []string, key, value string) []string {
	return envWithForOS(env, key, value, runtime.GOOS)
}

func envWithForOS(env []string, key, value, goos string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		same := envname.Same(goos, name, key)
		if goos == runtime.GOOS {
			same = gitx.SameEnvName(name, key)
		}
		if !same {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}
