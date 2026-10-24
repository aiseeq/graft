// Package workers names the models whose work a commit carries. A tool that
// runs a coding agent in a worktree leaves a marker in the repository while
// the job lives, <git common dir>/worker/<id>, a JSON object; a commit in
// that worktree gets a Worker trailer per model.
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
	"github.com/aiseeq/graft/v2/internal/proc"
)

// TrailerKey is the trailer a model is named in.
const TrailerKey = "Worker"

// Marker is a job: the models it ran, in order, in the worktree at Dir,
// while the process PID, whose command line names ID, lives, or after its
// round is Finished, its work not yet taken.
type Marker struct {
	ID       string   `json:"id"`
	Models   []string `json:"models"`
	Client   string   `json:"client"`
	Dir      string   `json:"dir"`
	PID      int      `json:"pid"`
	Finished bool     `json:"finished"`
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
	if m.ID == "" || len(m.Models) == 0 || slices.Contains(m.Models, "") || m.Dir == "" || (m.PID <= 0 && !m.Finished) {
		return Marker{}, errors.New("id, models, dir and a pid or finished are required")
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
//   - the markers of this worktree whose job runs or whose round is
//     finished: the master commits its work after the job's process exits;
//   - the Worker trailers of the commits a merge or a squash brings in.
//
// The markers of other worktrees name no model: two jobs changing the same
// file for their own reasons is common, and the work of one carried over
// by hand cannot be told from that. A marker whose worktree is gone or
// cannot be read, and one of a round not finished whose process is gone,
// the job killed and the marker left, is reported to warn and passed over.
func Models(repo *gitx.Repo, warn io.Writer) ([]string, error) {
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
	for _, m := range Markers(repo, warn) {
		dir, err := realPath(m.Dir)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(warn, "graft: the worker marker %s names %s, which is gone: passed over\n", m.file, m.Dir)
			continue
		}
		if err != nil {
			passOver(warn, m.file, err)
			continue
		}
		if dir != root {
			continue
		}
		if !m.Finished {
			runs, err := m.runs()
			if err != nil {
				passOver(warn, m.file, err)
				continue
			}
			if !runs {
				fmt.Fprintf(warn, "graft: the worker marker %s names the process %d, which is gone, and its round is not finished: the job was killed, passed over\n", m.file, m.PID)
				continue
			}
		}
		add(m.Models...)
	}
	merged, err := mergedModels(repo)
	if err != nil {
		return nil, err
	}
	add(merged...)
	return models, nil
}

// runs reports whether the job's process lives. A pid reused by another
// process after the job died is told apart where the platform can: the
// command line of the job's process names its id.
func (m Marker) runs() (bool, error) {
	alive, err := proc.Alive(m.PID)
	if err != nil || !alive {
		return false, err
	}
	argv, err := proc.Cmdline(m.PID)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil // exited meanwhile
	case err != nil:
		return false, err
	}
	return slices.Contains(argv, m.ID), nil
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
