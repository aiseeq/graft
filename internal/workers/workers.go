// Package workers names the models whose work a commit carries. A tool that
// runs a coding agent in a worktree leaves a marker in the repository while
// the job lives, <git common dir>/worker/<id>, a JSON object; a commit in
// that worktree, and one that stages the files of a finished job as it left
// them, gets a Worker trailer per model.
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
//   - the markers of other worktrees whose round is finished and some of
//     whose files the commit stages as the job left them (carried): the
//     master carried the job's diff over by hand;
//   - the Worker trailers of the commits a merge or a squash brings in.
//
// The marker of another worktree whose job still runs names no model, nor
// does one whose files the commit changes otherwise: two jobs changing the
// same file for their own reasons is common. A marker whose worktree is
// gone or cannot be read, and one of a round not finished whose process is
// gone, the job killed and the marker left, is reported to warn and passed
// over.
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
	var others []Marker
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
			if m.Finished {
				m.Dir = dir
				others = append(others, m)
			}
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
	if len(others) > 0 {
		staged, err := stagedBlobs(repo)
		if err != nil {
			return nil, err
		}
		for _, m := range others {
			n, err := carried(repo, m.Dir, staged)
			if err != nil {
				passOver(warn, m.file, err)
				continue
			}
			if n > 0 {
				fmt.Fprintf(warn, "graft: the commit stages %d files as the finished job %s left them in %s: Worker %s\n", n, m.ID, m.Dir, strings.Join(m.Models, ", "))
				add(m.Models...)
			}
		}
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

// stagedBlobs are the blobs of the regular files the commit adds or
// changes, by path.
func stagedBlobs(repo *gitx.Repo) (map[string]string, error) {
	out, err := repo.Git("diff", "--cached", "--raw", "--no-renames", "--no-abbrev", "-z")
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// :<old mode> <new mode> <old blob> <new blob> <status>
		meta := strings.Fields(fields[i])
		if len(meta) == 5 && (meta[1] == "100644" || meta[1] == "100755") && meta[4] != "D" {
			blobs[fields[i+1]] = meta[3]
		}
	}
	return blobs, nil
}

// carried counts the files of staged the worktree at dir holds with the
// same content and changed since it forked from HEAD: the job's work, not
// a file it left as it found it.
func carried(repo *gitx.Repo, dir string, staged map[string]string) (int, error) {
	var paths []string
	for p := range staged {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("the job's copy of %s: %w", p, err)
		}
		if info.Mode().IsRegular() && !strings.Contains(p, "\n") {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return 0, nil
	}
	slices.Sort(paths)
	out, err := gitx.InOther(dir, strings.NewReader(strings.Join(paths, "\n")+"\n"), "hash-object", "--stdin-paths")
	if err != nil {
		return 0, err
	}
	hashes := strings.Fields(out)
	if len(hashes) != len(paths) {
		return 0, fmt.Errorf("git hash-object hashed %d of %d files", len(hashes), len(paths))
	}
	var same []string
	for i, p := range paths {
		if hashes[i] == staged[p] {
			same = append(same, p)
		}
	}
	if len(same) == 0 {
		return 0, nil
	}
	forked, err := forkBlobs(repo, dir, same)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range same {
		// A file not there at the fork is the job's own.
		if blob, ok := forked[p]; !ok || blob != staged[p] {
			n++
		}
	}
	return n, nil
}

// forkBlobs are the blobs of paths where the worktree at dir forked from
// HEAD, by path; none when the two share no history.
func forkBlobs(repo *gitx.Repo, dir string, paths []string) (map[string]string, error) {
	if ok, err := repo.HasHead(); err != nil || !ok {
		return map[string]string{}, err
	}
	head, err := gitx.InOther(dir, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return nil, err
	}
	fork, err := repo.Git("merge-base", "HEAD", strings.TrimSpace(head))
	if gitx.ExitCode(err) == 1 {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := repo.Git(append([]string{"ls-tree", "-z", "--full-tree", strings.TrimSpace(fork), "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for entry := range strings.SplitSeq(out, "\x00") {
		// <mode> <type> <object>\t<path>
		meta, path, ok := strings.Cut(entry, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			blobs[path] = f[2]
		}
	}
	return blobs, nil
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
