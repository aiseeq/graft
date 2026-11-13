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
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/proc"
)

// TrailerKey is the trailer a model is named in.
const TrailerKey = "Worker"

// Marker is a job: the models it ran, in order, in the worktree at Dir,
// while the process PID, whose command line names ID, lives, or after its
// round is Finished, its work not yet taken.
type Marker struct {
	ID           string             `json:"id"`
	Models       []string           `json:"models"`
	Client       string             `json:"client"`
	Dir          string             `json:"dir"`
	PID          int                `json:"pid"`
	Finished     bool               `json:"finished"`
	FighterFiles *map[string]string `json:"fighter_files"`
	// file is where the marker was read, for messages.
	file string
	// written is when the marker was last written: for a finished round,
	// when the round ended, the tool writing it then.
	written time.Time
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
	info, err := os.Stat(file)
	if err != nil {
		return Marker{}, fmt.Errorf("reading: %w", err)
	}
	m.file, m.written = file, info.ModTime()
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
//   - the markers of other worktrees whose round is finished and whose
//     fighter_files list has files the commit stages with the listed blob,
//     where that blob is not the trunk's at that path, or deletes where the
//     trunk has the file (or whose older markers have some files the commit
//     stages as the job left them, with the same trunk check): the master
//     carried the job's diff over by hand; project version files and Go
//     module and workspace files do not count as carried work. With the
//     trunk unknown the trunk check is skipped;
//   - the Worker trailers of the commits a merge or a squash brings in.
//
// The marker of another worktree whose job still runs names no model, nor
// does one whose files the commit changes otherwise: two jobs changing the
// same file for their own reasons is common. A marker whose worktree is
// gone or cannot be read is reported to warn and passed over, unless a
// finished marker has fighter_files; one of a round not finished whose
// process is gone, the job killed and the marker left, is also reported to
// warn and passed over.
//
// trunk names the trunk ref, "" when unknown; it is asked only when another
// worktree's finished marker is in question.
func Models(repo *gitx.Repo, trunk func() string, warn io.Writer, excluded []string) ([]string, error) {
	root, err := realPath(repo.Root)
	if err != nil {
		return nil, err
	}
	models, others := markerModels(repo, warn, root)
	if len(others) > 0 {
		staged, deleted, err := stagedBlobs(repo)
		if err != nil {
			return nil, err
		}
		trunkFiles, err := treeBlobs(repo, trunk(), staged, deleted)
		if err != nil {
			return nil, err
		}
		for i := range others {
			m := others[i]
			var n int
			if m.FighterFiles != nil {
				n = listedFiles(*m.FighterFiles, staged, deleted, trunkFiles, excluded)
			} else {
				n, err = carried(repo, m.Dir, m.written, staged, trunkFiles, excluded)
			}
			if err != nil {
				passOver(warn, m.file, err)
				continue
			}
			if n > 0 {
				if m.FighterFiles != nil {
					fmt.Fprintf(warn, "graft: the commit stages %d files as the finished job %s left them in %s by its file list: Worker %s\n", n, m.ID, m.Dir, strings.Join(m.Models, ", "))
				} else {
					fmt.Fprintf(warn, "graft: the commit stages %d files as the finished job %s left them in %s: Worker %s\n", n, m.ID, m.Dir, strings.Join(m.Models, ", "))
				}
				addModels(&models, m.Models...)
			}
		}
	}
	merged, err := mergedModels(repo)
	if err != nil {
		return nil, err
	}
	addModels(&models, merged...)
	return models, nil
}

func markerModels(repo *gitx.Repo, warn io.Writer, root string) ([]string, []Marker) {
	var models []string
	var others []Marker
	markers := Markers(repo, warn)
	for i := range markers {
		m := markers[i]
		dir, err := realPath(m.Dir)
		if errors.Is(err, os.ErrNotExist) {
			if m.Finished && m.FighterFiles != nil {
				others = append(others, m)
				continue
			}
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
		addModels(&models, m.Models...)
	}
	return models, others
}

func addModels(models *[]string, ms ...string) {
	for _, m := range ms {
		if !slices.Contains(*models, m) {
			*models = append(*models, m)
		}
	}
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
// changes, by path, and the regular files it deletes.
func stagedBlobs(repo *gitx.Repo) (map[string]string, map[string]bool, error) {
	out, err := repo.Git("diff", "--cached", "--raw", "--no-renames", "--no-abbrev", "-z")
	if err != nil {
		return nil, nil, err
	}
	blobs := map[string]string{}
	deleted := map[string]bool{}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// :<old mode> <new mode> <old blob> <new blob> <status>
		meta := strings.Fields(fields[i])
		if len(meta) == 5 {
			if meta[4] == "D" && (meta[0] == ":100644" || meta[0] == ":100755") {
				deleted[fields[i+1]] = true
			} else if (meta[1] == "100644" || meta[1] == "100755") && meta[4] != "D" {
				blobs[fields[i+1]] = meta[3]
			}
		}
	}
	return blobs, deleted, nil
}

func listedFiles(files map[string]string, staged map[string]string, deleted map[string]bool, trunk map[string]string, excluded []string) int {
	n := 0
	for p, oid := range files {
		if slices.Contains(excluded, p) || slices.Contains(goModuleFiles, path.Base(p)) {
			continue
		}
		if oid == "" {
			if deleted[p] {
				if _, inTrunk := trunk[p]; inTrunk || trunk == nil {
					n++
				}
			}
		} else if staged[p] == oid && differsFromTrunk(oid, trunk, p) {
			n++
		}
	}
	return n
}

// goModuleFiles are the names of the files graft commit and the go command
// rewrite alike in every worktree (modules.latest, go mod tidy).
var goModuleFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// carried counts the files of staged but excluded and goModuleFiles the
// worktree at dir holds with the same content and changed since it forked
// from HEAD: the job's work, not a file it left as it found it, nor one
// graft writes the same way in both trees, nor one changed there after
// the round ended at ended.
func carried(repo *gitx.Repo, dir string, ended time.Time, staged, trunk map[string]string, excluded []string) (int, error) {
	var paths []string
	for p := range staged {
		if slices.Contains(excluded, p) || slices.Contains(goModuleFiles, path.Base(p)) {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("the job's copy of %s: %w", p, err)
		}
		// A file changed after the round ended is not the job's: the master
		// edited it in the job's tree by hand.
		if info.Mode().IsRegular() && !strings.Contains(p, "\n") && !info.ModTime().After(ended) {
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
		if hashes[i] != staged[p] {
			continue
		}
		if differsFromTrunk(staged[p], trunk, p) {
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

// differsFromTrunk reports whether blob is absent from trunk at path or differs there.
func differsFromTrunk(blob string, trunk map[string]string, path string) bool {
	trunkBlob, exists := trunk[path]
	return !exists || blob != trunkBlob
}

// treeBlobs are the blobs at the listed paths in trunk; absent paths have no
// entry, including files deleted from trunk. An unknown trunk ("") gives
// nil, which differsFromTrunk and listedFiles read as "no trunk to check".
func treeBlobs(repo *gitx.Repo, trunk string, staged map[string]string, deleted map[string]bool) (map[string]string, error) {
	paths := make([]string, 0, len(staged)+len(deleted))
	for p := range staged {
		paths = append(paths, p)
	}
	for p := range deleted {
		paths = append(paths, p)
	}
	if trunk == "" {
		return nil, nil
	}
	if len(paths) == 0 {
		return map[string]string{}, nil
	}
	out, err := repo.Git(append([]string{"ls-tree", "-z", "--full-tree", trunk, "--"}, paths...)...)
	if err != nil {
		return nil, err
	}
	blobs := map[string]string{}
	for entry := range strings.SplitSeq(out, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 3 && fields[1] == "blob" {
			blobs[name] = fields[2]
		}
	}
	return blobs, nil
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
		meta, name, ok := strings.Cut(entry, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			blobs[name] = f[2]
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
