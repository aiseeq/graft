// Package npmmods raises pinned npm git dependencies selected by
// modules.latest.
package npmmods

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jsonspan"
	"golang.org/x/mod/semver"
)

// Raise updates matching semver-tagged git dependencies in package manifests,
// and refreshes an adjacent or covering workspace lock file.
func Raise(ctx context.Context, repo *gitx.Repo, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	files, err := repo.Git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("listing tracked files: %w", err)
	}
	drop, err := gitx.LocalEnvVars()
	if err != nil {
		return fmt.Errorf("listing git environment variables: %w", err)
	}
	env := gitx.Without(os.Environ(), drop)
	for name := range strings.SplitSeq(files, "\x00") {
		if filepath.Base(name) != "package.json" || excludedPath(name) {
			continue
		}
		if err := raiseManifest(ctx, repo.Root, name, env, own, log, warning); err != nil {
			return err
		}
	}
	return nil
}

type pin struct {
	name, url, tag string
	start, end     int
	latest         string
}

func raiseManifest(ctx context.Context, root, name string, env []string, own func(string) bool, log io.Writer, warning func(string, ...any)) error {
	display := filepath.ToSlash(name)
	path := filepath.Join(root, filepath.FromSlash(name))
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", display, err)
	}
	pins, err := packagePins(data, own)
	if err != nil {
		return fmt.Errorf("%s: parsing manifest: %w", display, err)
	}
	raised := latestPins(ctx, display, env, pins, warning)
	if len(raised) == 0 {
		return nil
	}
	npm, err := exec.LookPath("npm")
	if errors.Is(err, exec.ErrNotFound) {
		var names []string
		for _, p := range raised {
			names = append(names, fmt.Sprintf("%s %s -> %s", p.name, p.tag, p.latest))
		}
		return fmt.Errorf("%s: npm is required to raise %s", display, strings.Join(names, ", "))
	}
	if err != nil {
		return fmt.Errorf("looking up npm: %w", err)
	}
	updated := rewrite(data, raised)
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", display, err)
	}
	lockDir, found, err := lockDir(filepath.Dir(path), root)
	if err != nil {
		return fmt.Errorf("checking %s package-lock.json: %w", display, err)
	}
	if found {
		if err := updateLock(ctx, display, lockDir, npm, env, raised); err != nil {
			return err
		}
	}
	for _, p := range raised {
		fmt.Fprintf(log, "graft: %s: %s %s -> %s\n", display, p.name, p.tag, p.latest)
	}
	return nil
}

func packagePins(data []byte, own func(string) bool) ([]pin, error) {
	var pkg struct {
		Dependencies         map[string]json.RawMessage `json:"dependencies"`
		DevDependencies      map[string]json.RawMessage `json:"devDependencies"`
		OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
		PeerDependencies     map[string]json.RawMessage `json:"peerDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("decoding package manifest: %w", err)
	}
	groups := []struct {
		key  string
		deps map[string]json.RawMessage
	}{{"dependencies", pkg.Dependencies}, {"devDependencies", pkg.DevDependencies}, {"optionalDependencies", pkg.OptionalDependencies}, {"peerDependencies", pkg.PeerDependencies}}
	var pins []pin
	for _, group := range groups {
		for name, rawSpec := range group.deps {
			var spec string
			if err := json.Unmarshal(rawSpec, &spec); err != nil {
				return nil, fmt.Errorf("dependency %q: expected a string spec: %w", name, err)
			}
			urlPart, tag, ok := parseGitSpec(spec)
			if !ok || !own(normalizeGitPath(urlPart)) {
				continue
			}
			spans, err := jsonspan.Locate(data, []string{group.key, name})
			if err != nil {
				return nil, err
			}
			span := spans[0]
			raw := data[span.Start:span.End]
			hash := bytes.LastIndexByte(raw, '#')
			if hash < 0 || string(raw[hash+1:]) != tag {
				continue
			}
			pins = append(pins, pin{name: name, url: urlPart, tag: tag, start: span.Start + hash + 1, end: span.End})
		}
	}
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].start != pins[j].start {
			return pins[i].start < pins[j].start
		}
		if pins[i].name != pins[j].name {
			return pins[i].name < pins[j].name
		}
		return pins[i].tag < pins[j].tag
	})
	return pins, nil
}

func parseGitSpec(spec string) (urlPart, tag string, ok bool) {
	i := strings.LastIndexByte(spec, '#')
	if i < 0 {
		return "", "", false
	}
	tag = spec[i+1:]
	if !validTag(tag) {
		return "", "", false
	}
	urlPart = spec[:i]
	return urlPart, tag, normalizeGitPath(urlPart) != ""
}

func normalizeGitPath(raw string) string {
	if strings.HasPrefix(raw, "github:") {
		return ""
	}
	raw = strings.TrimPrefix(raw, "git+")
	var host, path string
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "git", "http", "https", "ssh":
		default:
			return ""
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	} else if i := strings.IndexByte(raw, ':'); i > 0 {
		host, path = raw[:i], strings.TrimPrefix(raw[i+1:], "/")
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
	}
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimPrefix(strings.ToLower(host), "ssh.")
	path = strings.TrimSuffix(path, ".git")
	if host == "" {
		return ""
	}
	return host + "/" + path
}

func validTag(tag string) bool { return strings.HasPrefix(tag, "v") && semver.IsValid(tag) }

func includesTag(current, candidate string) bool {
	return validTag(candidate) && (semver.Prerelease(current) != "" || semver.Prerelease(candidate) == "")
}

func shouldRaise(current, latest string) bool {
	return validTag(current) && validTag(latest) && semver.Compare(latest, current) > 0
}

func latestPins(ctx context.Context, display string, env []string, pins []pin, warning func(string, ...any)) []pin {
	var raised []pin
	for _, p := range pins {
		latest, found, err := latestTag(ctx, env, p.url, p.tag)
		if err != nil {
			warning("%s: %s: the latest tag is unknown: %v", display, p.name, err)
			continue
		}
		if found && shouldRaise(p.tag, latest) {
			p.latest = latest
			raised = append(raised, p)
		}
	}
	return raised
}

// lsRemoteTimeout bounds asking a repository for its tags, so a host that
// does not answer delays a commit by this much, not forever.
const lsRemoteTimeout = time.Minute

func latestTag(ctx context.Context, env []string, rawURL, current string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lsRemoteTimeout)
	defer cancel()
	stdout, stderr, err := execx.Run(ctx, "", "git", env, "ls-remote", "--tags", strings.TrimPrefix(rawURL, "git+"))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", false, fmt.Errorf("git ls-remote: no answer in %s", lsRemoteTimeout)
		}
		if strings.TrimSpace(stderr) == "" {
			stderr = err.Error()
		}
		return "", false, fmt.Errorf("git ls-remote: %s", strings.TrimSpace(stderr))
	}
	var best string
	for line := range strings.SplitSeq(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/tags/") {
			continue
		}
		tag := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/tags/"), "^{}")
		if includesTag(current, tag) && (best == "" || semver.Compare(tag, best) > 0 || (semver.Compare(tag, best) == 0 && tag < best)) {
			best = tag
		}
	}
	return best, best != "", nil
}

func rewrite(data []byte, pins []pin) []byte {
	updated := bytes.Clone(data)
	for i := len(pins) - 1; i >= 0; i-- {
		p := pins[i]
		updated = append(updated[:p.start], append([]byte(p.latest), updated[p.end:]...)...)
	}
	return updated
}

func updateLock(ctx context.Context, display, dir, npm string, env []string, pins []pin) error {
	out, stderr, err := execx.Run(ctx, dir, npm, env, "install", "--package-lock-only")
	if err != nil {
		return npmFailure(display, "npm install --package-lock-only", stderr, out, err)
	}
	seen := make(map[string]bool)
	for _, p := range pins {
		if seen[p.name] {
			continue
		}
		seen[p.name] = true
		out, stderr, err = execx.Run(ctx, dir, npm, env, "update", p.name, "--package-lock-only")
		if err != nil {
			return npmFailure(display, "npm update "+p.name+" --package-lock-only", stderr, out, err)
		}
	}
	return nil
}

func excludedPath(name string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(name), "/") {
		if part == "testdata" || part == "vendor" || part == "node_modules" {
			return true
		}
	}
	return false
}

func lockDir(packageDir, root string) (string, bool, error) {
	for dir := packageDir; ; dir = filepath.Dir(dir) {
		lockPath := filepath.Join(dir, "package-lock.json")
		_, err := os.Stat(lockPath)
		if err == nil {
			if dir == packageDir {
				return dir, true, nil
			}
			covered, err := lockCovers(lockPath, packageDir, dir)
			if err != nil {
				return "", false, err
			}
			if covered {
				return dir, true, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		if dir == root {
			return "", false, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
	}
}

func lockCovers(lockPath, packageDir, lockDir string) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", lockPath, err)
	}
	var lock struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return false, fmt.Errorf("decoding %s: %w", lockPath, err)
	}
	rel, err := filepath.Rel(lockDir, packageDir)
	if err != nil {
		return false, fmt.Errorf("relative package path: %w", err)
	}
	_, ok := lock.Packages[filepath.ToSlash(rel)]
	return ok, nil
}

func npmFailure(display, command, stderr, stdout string, err error) error {
	var output []string
	for _, part := range []string{strings.TrimSpace(stderr), strings.TrimSpace(stdout)} {
		if part != "" {
			output = append(output, part)
		}
	}
	return fmt.Errorf("%s: %s failed: %w\n%s", display, command, err, strings.Join(output, "\n"))
}
