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
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"golang.org/x/mod/modfile"
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
	goPath, err := exec.LookPath("go")
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up go: %w", err)
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
		if err := raiseFile(ctx, filepath.Join(repo.Root, filepath.FromSlash(name)), name, goPath, env, own, log, warning); err != nil {
			return err
		}
	}
	return nil
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
	out, stderr, err := runGo(ctx, filepath.Dir(path), goPath, env, args...)
	if err != nil {
		return goFailure(display, "go get", stderr, out, err)
	}
	for _, c := range upgraded {
		fmt.Fprintf(log, "graft: %s: %s %s -> %s\n", display, c.path, c.version, latest[c.path].Version)
	}
	out, stderr, err = runGo(ctx, filepath.Dir(path), goPath, env, "mod", "tidy")
	if err != nil {
		return goFailure(display, "go mod tidy", stderr, out, err)
	}
	_, statErr := os.Stat(filepath.Join(filepath.Dir(path), "vendor", "modules.txt"))
	if statErr == nil {
		out, stderr, err = runGo(ctx, filepath.Dir(path), goPath, env, "mod", "vendor")
		if err != nil {
			return goFailure(display, "go mod vendor", stderr, out, err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking %s vendor/modules.txt: %w", display, statErr)
	}
	return nil
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
	out, stderr, err := runGo(ctx, dir, goPath, env, args...)
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

func runGo(ctx context.Context, dir, goPath string, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, goPath, args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
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
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, key) {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}
