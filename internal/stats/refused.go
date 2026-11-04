package stats

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// RefusedCommit is a raw git commit the pre-commit hook refused: what counts
// how often agents go past graft commit, and tells whom to ask why.
type RefusedCommit struct {
	Time time.Time `json:"time"`
	// Session is the agent session that ran it; absent outside one.
	Session string `json:"session,omitempty"`
	// Project is the absolute root of the work tree.
	Project string `json:"project"`
	// Branch is absent on a detached HEAD.
	Branch string `json:"branch,omitempty"`
}

// RefusedCommitsPath is the file of refused commits next to the stats file at
// path, apart from it: other tools read the stats file line by line.
func RefusedCommitsPath(path string) string {
	return filepath.Join(filepath.Dir(path), "refused-commits.jsonl")
}

// AppendRefusedCommit records a refused commit.
func AppendRefusedCommit(rec RefusedCommit) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return Append(RefusedCommitsPath(path), rec, MaxSize)
}

// LoadRefusedCommits reads the refused commits since a time, of one project
// unless project is empty, newest first, and counts the lines it could not read.
func LoadRefusedCommits(since time.Time, project string) ([]RefusedCommit, int, error) {
	path, err := Path()
	if err != nil {
		return nil, 0, err
	}
	files, err := Files(RefusedCommitsPath(path), since)
	if err != nil {
		return nil, 0, err
	}
	var recs []RefusedCommit
	malformed := 0
	for _, file := range files {
		got, bad, err := loadRefusedFile(file)
		if err != nil {
			return nil, 0, err
		}
		malformed += bad
		for _, r := range got {
			if !r.Time.Before(since) && (project == "" || r.Project == project) {
				recs = append(recs, r)
			}
		}
	}
	slices.SortStableFunc(recs, func(a, b RefusedCommit) int { return b.Time.Compare(a.Time) })
	return recs, malformed, nil
}

func loadRefusedFile(path string) (recs []RefusedCommit, malformed int, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("reading the refused commits: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r RefusedCommit
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Project == "" {
			malformed++
			continue
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	return recs, malformed, nil
}
