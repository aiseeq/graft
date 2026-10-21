// Package workers names the models whose work a commit carries. A tool that
// runs a coding agent in a worktree leaves a marker in the repository while
// the job lives, <git common dir>/worker/<id>, a JSON object; the commit
// gets a Worker trailer per model.
package workers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

// TrailerKey is the trailer a model is named in.
const TrailerKey = "Worker"

// Marker is a live job: the models it ran, in order, in the worktree at Dir,
// which started at the commit Base.
type Marker struct {
	ID     string   `json:"id"`
	Models []string `json:"models"`
	Client string   `json:"client"`
	Dir    string   `json:"dir"`
	Base   string   `json:"base"`
	// file is where the marker was read, for messages.
	file string
}

// markerDir holds the markers of a repository, shared by its worktrees.
func markerDir(repo *gitx.Repo) string { return filepath.Join(repo.CommonDir, "worker") }

// Markers reads the live markers; names starting with a dot are files being
// written. A marker is a hint, not a part of the commit: one that cannot be
// read, does not parse or names no model is reported to warn and passed over.
func Markers(repo *gitx.Repo, warn io.Writer) []Marker {
	entries, err := os.ReadDir(markerDir(repo))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		passOver(warn, markerDir(repo), err)
		return nil
	}
	var markers []Marker
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			continue
		}
		file := filepath.Join(markerDir(repo), e.Name())
		m, err := readMarker(file)
		if err != nil {
			passOver(warn, file, err)
			continue
		}
		markers = append(markers, m)
	}
	return markers
}

func readMarker(file string) (Marker, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Marker{}, fmt.Errorf("reading: %w", err)
	}
	var m Marker
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return Marker{}, fmt.Errorf("not JSON: %w", err)
	}
	if len(m.Models) == 0 || slices.Contains(m.Models, "") || m.Dir == "" || m.Base == "" {
		return Marker{}, errors.New("models, dir and base are required")
	}
	m.file = file
	return m, nil
}

// passOver reports a marker the commit goes without.
func passOver(warn io.Writer, file string, err error) {
	fmt.Fprintf(warn, "graft: the worker marker %s: %v: passed over, the commit names no model of it\n", file, err)
}

// Models are the models whose work the commit being made in repo carries,
// in order, each once:
//   - the markers of this worktree, where the job runs;
//   - the markers of other worktrees whose changes since their base, made
//     or not yet committed, touch a staged file: the job's work carried over
//     by hand. The files in skip (the version files a commit bumps) do not
//     count, every commit touches them;
//   - the Worker trailers of the commits a merge or a squash brings in.
//
// A marker whose worktree is gone or cannot be read is reported to warn
// and passed over.
func Models(repo *gitx.Repo, skip []string, warn io.Writer) ([]string, error) {
	markers := Markers(repo, warn)
	var models []string
	add := func(ms ...string) {
		for _, m := range ms {
			if !slices.Contains(models, m) {
				models = append(models, m)
			}
		}
	}
	root, err := realPath(repo.Root)
	if err != nil {
		return nil, err
	}
	var staged []string
	for _, m := range markers {
		dir, err := realPath(m.Dir)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(warn, "graft: the worker marker %s names %s, which is gone: passed over\n", m.file, m.Dir)
			continue
		}
		if err != nil {
			passOver(warn, m.file, err)
			continue
		}
		if dir == root {
			add(m.Models...)
			continue
		}
		if staged == nil {
			if staged, err = stagedFiles(repo, skip); err != nil {
				return nil, err
			}
		}
		changed, err := changedSince(dir, m.Base)
		if err != nil {
			passOver(warn, m.file, err)
			continue
		}
		if slices.ContainsFunc(staged, func(f string) bool { return changed[f] }) {
			add(m.Models...)
		}
	}
	merged, err := mergedModels(repo)
	if err != nil {
		return nil, err
	}
	add(merged...)
	return models, nil
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("the path %s: %w", p, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("the path %s: %w", p, err)
	}
	return real, nil
}

// stagedFiles are the files the commit changes, but those in skip; never
// nil.
func stagedFiles(repo *gitx.Repo, skip []string) ([]string, error) {
	out, err := repo.Git("diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	files := []string{}
	for f := range strings.SplitSeq(out, "\x00") {
		if f != "" && !slices.Contains(skip, f) {
			files = append(files, f)
		}
	}
	return files, nil
}

// changedSince are the files the worktree at dir changed since base: in
// its commits and not yet committed, untracked ones too. The tree is
// another worktree: git there must not inherit a hook's index.
func changedSince(dir, base string) (map[string]bool, error) {
	tree, err := gitx.OpenOther(dir)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "--no-renames", "-z", base},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		out, err := tree.Git(args...)
		if err != nil {
			return nil, err
		}
		for f := range strings.SplitSeq(out, "\x00") {
			if f != "" {
				changed[f] = true
			}
		}
	}
	return changed, nil
}

// squashedCommit is a commit line of SQUASH_MSG, which git merge --squash
// writes with the log of what it brings in.
var squashedCommit = regexp.MustCompile(`(?m)^commit ([0-9a-f]{40,64})$`)

// mergedModels are the Worker trailers of the commits a merge in progress
// (MERGE_HEAD) or a squash (SQUASH_MSG) brings in.
func mergedModels(repo *gitx.Repo) ([]string, error) {
	var logs [][]string
	heads, err := os.ReadFile(filepath.Join(repo.GitDir, "MERGE_HEAD"))
	switch {
	case err == nil:
		logs = append(logs, append(strings.Fields(string(heads)), "--not", "HEAD"))
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("MERGE_HEAD: %w", err)
	}
	msg, err := os.ReadFile(filepath.Join(repo.GitDir, "SQUASH_MSG"))
	switch {
	case err == nil:
		var commits []string
		for _, m := range squashedCommit.FindAllStringSubmatch(string(msg), -1) {
			commits = append(commits, m[1])
		}
		if len(commits) > 0 {
			logs = append(logs, append([]string{"--no-walk=unsorted"}, commits...))
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("SQUASH_MSG: %w", err)
	}
	var models []string
	for _, revs := range logs {
		out, err := repo.Git(append([]string{"log", "--format=%(trailers:key=" + TrailerKey + ",valueonly,unfold,separator=%x00)%x00"}, revs...)...)
		if err != nil {
			return nil, err
		}
		for m := range strings.SplitSeq(out, "\x00") {
			if m = strings.TrimSpace(m); m != "" && !slices.Contains(models, m) {
				models = append(models, m)
			}
		}
	}
	return models, nil
}

// WithTrailers is msg with a Worker trailer for each of models it does not
// name yet, where git puts trailers.
func WithTrailers(repo *gitx.Repo, msg string, models []string) (string, error) {
	if len(models) == 0 {
		return msg, nil
	}
	args := []string{"interpret-trailers", "--if-exists", "addIfDifferent"}
	for _, m := range models {
		args = append(args, "--trailer", TrailerKey+": "+m)
	}
	return repo.GitInput(strings.NewReader(msg), nil, args...)
}
