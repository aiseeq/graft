package leaks

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/internal/gitx"
)

// Line is a line a change adds.
type Line struct {
	Path string
	No   int
	Text string
}

// diffArgs make git print every added line with its number in the new file,
// paths unquoted.
var diffArgs = []string{"-U0", "--no-color", "--no-ext-diff", "--no-renames", "--diff-filter=AMT", "--src-prefix=a/", "--dst-prefix=b/"}

// StagedLines returns the lines the index adds to HEAD.
func StagedLines(repo *gitx.Repo) ([]Line, error) {
	out, err := repo.Git(append([]string{"-c", "core.quotePath=false", "diff", "--cached"}, diffArgs...)...)
	if err != nil {
		return nil, err
	}
	return ParseDiff(out)
}

// CommitLines returns the lines a commit adds to its first parent.
func CommitLines(repo *gitx.Repo, commit string) ([]Line, error) {
	out, err := repo.Git(append([]string{"-c", "core.quotePath=false", "diff-tree", "-p", "-r", "--root", "--no-commit-id"}, append(diffArgs, commit)...)...)
	if err != nil {
		return nil, err
	}
	return ParseDiff(out)
}

// ParseDiff reads the added lines of a unified diff with zero context. Hunk
// line counts, not prefixes, delimit the content: an added "++ x" line looks
// like a file header.
func ParseDiff(out string) ([]Line, error) {
	var lines []Line
	var path string
	no, left := 0, 0
	for raw := range strings.SplitSeq(out, "\n") {
		if left > 0 {
			switch {
			case strings.HasPrefix(raw, "+"):
				lines = append(lines, Line{Path: path, No: no, Text: raw[1:]})
				no++
				left--
			case strings.HasPrefix(raw, `\`): // \ No newline at end of file
			case strings.HasPrefix(raw, "-"):
			default:
				return nil, fmt.Errorf("diff of %s: unexpected line %q inside a hunk", path, raw)
			}
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+++ b/"):
			path = strings.TrimPrefix(raw, "+++ b/")
		case strings.HasPrefix(raw, "+++ "):
			path = ""
		case strings.HasPrefix(raw, "@@ "):
			start, count, err := hunkNew(raw)
			if err != nil {
				return nil, fmt.Errorf("diff of %s: %w", path, err)
			}
			no, left = start, count
		}
	}
	if left > 0 {
		return nil, fmt.Errorf("diff of %s ends inside a hunk", path)
	}
	return lines, nil
}

// hunkNew reads "+start,count" from "@@ -a,b +c,d @@".
func hunkNew(header string) (int, int, error) {
	fields := strings.Fields(header)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, fmt.Errorf("bad hunk header %q", header)
	}
	startText, countText, hasCount := strings.Cut(fields[2][1:], ",")
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, fmt.Errorf("bad hunk header %q", header)
	}
	count := 1
	if hasCount {
		if count, err = strconv.Atoi(countText); err != nil {
			return 0, 0, fmt.Errorf("bad hunk header %q", header)
		}
	}
	return start, count, nil
}

// Finding is a private name or term in a change.
type Finding struct {
	Path   string
	Line   int
	Reason string
}

// MessagePath names the commit message in findings.
const MessagePath = "commit message"

// Scan checks the added lines and the commit message. A private name the
// repository declares in its own non-test Go code, at base or in the added
// lines, is its own vocabulary; a mention anywhere else (tests, fixtures,
// comments, docs, the message) is a leak. Terms are never the repository's
// own. base "" means there is nothing before.
func (r *Rules) Scan(repo *gitx.Repo, base string, lines []Line, message string) ([]Finding, error) {
	for i, text := range strings.Split(message, "\n") {
		lines = append(lines, Line{Path: MessagePath, No: i + 1, Text: text})
	}
	var own map[string]bool
	isOwn := func(name string) (bool, error) {
		if own == nil {
			var err error
			if own, err = ownNames(repo, base, lines); err != nil {
				return false, err
			}
		}
		return own[name], nil
	}
	var findings []Finding
	for _, l := range lines {
		hits, err := r.ScanLine(l.Text)
		if err != nil {
			findings = append(findings, Finding{Path: l.Path, Line: l.No, Reason: err.Error()})
			continue
		}
		for _, h := range hits {
			if h.Name != "" {
				ownName, err := isOwn(h.Name)
				if err != nil {
					return nil, err
				}
				if ownName {
					continue
				}
			}
			findings = append(findings, Finding{Path: l.Path, Line: l.No, Reason: h.Reason})
		}
	}
	return findings, nil
}

// maxOwnBlob bounds the Go files read for the repository's own names.
const maxOwnBlob = 4 << 20

// ownNames returns the names declared in the repository's non-test Go code at
// base and in the added lines.
func ownNames(repo *gitx.Repo, base string, lines []Line) (map[string]bool, error) {
	own := map[string]bool{}
	if base != "" {
		out, err := repo.Git("ls-tree", "-r", "-z", "--name-only", base)
		if err != nil {
			return nil, err
		}
		var specs []string
		for p := range strings.SplitSeq(out, "\x00") {
			if isOwnGo(p) {
				specs = append(specs, base+":"+p)
			}
		}
		blobs, err := repo.ReadBlobs(specs, maxOwnBlob)
		if err != nil {
			return nil, err
		}
		for _, b := range blobs {
			for _, n := range declarations(string(b.Content)) {
				own[n] = true
			}
		}
	}
	added := map[string]*strings.Builder{}
	for _, l := range lines {
		if !isOwnGo(l.Path) {
			continue
		}
		if added[l.Path] == nil {
			added[l.Path] = &strings.Builder{}
		}
		added[l.Path].WriteString(l.Text + "\n")
	}
	for _, b := range added {
		for _, n := range declarations(b.String()) {
			own[n] = true
		}
	}
	return own, nil
}

// isOwnGo is a Go file of the program itself: not a test, not test data.
func isOwnGo(p string) bool {
	return strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") &&
		!strings.HasPrefix(p, "testdata/") && !strings.Contains(p, "/testdata/")
}
