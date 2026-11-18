package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

const StaticcheckSource = "staticcheck -checks 'SA*' -f json ./..."

const staticcheckHint = "only staticcheck SA checks on changed lines are enforced; install with go install honnef.co/go/tools/cmd/staticcheck@latest or use graft tools"

type staticcheckDiagnostic struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Location struct {
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"location"`
}

type changedGoFile struct {
	paths       []string
	destination string
}

type parsedStaticcheck struct {
	diagnosticLines []string
	findings        []string
	nonSA           []string
}

// changedLines returns added or changed lines against HEAD, with staged and
// unstaged edits combined. New untracked Go files count in full.
func (r *Runner) changedLines(ctx context.Context) (map[string]map[int]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("getting changed lines against HEAD: %w", err)
	}
	repo, err := r.repo()
	if err != nil {
		return nil, fmt.Errorf("opening repository for changed lines: %w", err)
	}
	changed := make(map[string]map[int]bool)
	tracked, err := repo.Git("diff", "--name-status", "-z", "-M", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("listing changed files against HEAD: %w", err)
	}
	records, err := parseChangedGoFiles(tracked)
	if err != nil {
		return nil, err
	}
	if err := r.addTrackedChangedLines(ctx, repo, records, changed); err != nil {
		return nil, err
	}
	if err := r.addUntrackedChangedLines(repo, changed); err != nil {
		return nil, err
	}
	return changed, nil
}

func parseChangedGoFiles(output string) ([]changedGoFile, error) {
	entries := strings.Split(output, "\x00")
	var files []changedGoFile
	for i := 0; i < len(entries); {
		status := entries[i]
		i++
		if status == "" {
			continue
		}
		if i >= len(entries) || entries[i] == "" {
			return nil, fmt.Errorf("parsing changed files: %s status has no path", status)
		}
		source := entries[i]
		i++
		destination := source
		paths := []string{source}
		if isRenameOrCopy(status) {
			if i >= len(entries) || entries[i] == "" {
				return nil, fmt.Errorf("parsing changed files: incomplete %s record", status)
			}
			destination = entries[i]
			i++
			paths = append(paths, destination)
		}
		if strings.HasPrefix(status, "D") || !strings.HasSuffix(destination, ".go") {
			continue
		}
		files = append(files, changedGoFile{paths: paths, destination: destination})
	}
	return files, nil
}

func isRenameOrCopy(status string) bool {
	return strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C")
}

func (r *Runner) addTrackedChangedLines(ctx context.Context, repo *gitx.Repo, files []changedGoFile, changed map[string]map[int]bool) error {
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("getting changed lines against HEAD: %w", err)
		}
		patch, err := changedFilePatch(repo, file.paths)
		if err != nil {
			return fmt.Errorf("getting changed lines for %s: %w", file.destination, err)
		}
		lines, err := changedHunkLines(patch)
		if err != nil {
			return fmt.Errorf("reading changed lines for %s: %w", file.destination, err)
		}
		for line := range lines {
			addChangedLine(changed, file.destination, line)
		}
	}
	return nil
}

func changedFilePatch(repo *gitx.Repo, paths []string) (string, error) {
	args := []string{"diff", "--no-ext-diff", "--unified=0", "-M", "HEAD", "--"}
	for _, name := range paths {
		args = append(args, ":(literal)"+name)
	}
	return repo.Git(args...)
}

func changedHunkLines(patch string) (map[int]bool, error) {
	lines := make(map[int]bool)
	for line := range strings.SplitSeq(patch, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		numbers, err := parseAddedHunkRange(line)
		if err != nil {
			return nil, err
		}
		for n := numbers.start; n < numbers.start+numbers.count; n++ {
			lines[n] = true
		}
	}
	return lines, nil
}

type hunkRange struct{ start, count int }

func parseAddedHunkRange(line string) (hunkRange, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return hunkRange{}, fmt.Errorf("invalid diff hunk header %q", line)
	}
	parts := strings.SplitN(strings.TrimPrefix(fields[2], "+"), ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return hunkRange{}, fmt.Errorf("invalid added line in hunk header %q: %w", line, err)
	}
	count := 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil {
			return hunkRange{}, fmt.Errorf("invalid added line count in hunk header %q: %w", line, err)
		}
	}
	return hunkRange{start: start, count: count}, nil
}

func (r *Runner) addUntrackedChangedLines(repo *gitx.Repo, changed map[string]map[int]bool) error {
	untracked, err := repo.Git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return fmt.Errorf("listing untracked files: %w", err)
	}
	for name := range strings.SplitSeq(untracked, "\x00") {
		if name == "" || !strings.HasSuffix(name, ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(name)))
		if errors.Is(err, os.ErrNotExist) {
			continue // It was removed after git listed it, so there are no lines to check.
		}
		if err != nil {
			return fmt.Errorf("reading untracked Go file %s: %w", name, err)
		}
		for line := 1; line <= sourceLineCount(data); line++ {
			addChangedLine(changed, name, line)
		}
	}
	return nil
}

func sourceLineCount(data []byte) int {
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

func addChangedLine(changed map[string]map[int]bool, file string, line int) {
	if changed[file] == nil {
		changed[file] = make(map[int]bool)
	}
	changed[file][line] = true
}

func (r *Runner) checkStaticcheck(ctx context.Context, module string) error {
	if _, err := exec.LookPath("staticcheck"); err != nil {
		return fmt.Errorf("staticcheck is not in PATH: %w; install with go install honnef.co/go/tools/cmd/staticcheck@latest or use graft tools", err)
	}
	changed, err := r.changedLines(ctx)
	if err != nil {
		return err
	}
	output, stderr, runErr := r.runStaticcheck(ctx, module)
	parsed, err := parseStaticcheckOutput(output, module, r.Root, changed)
	if err != nil {
		return err
	}
	exitCode, fail := staticcheckFailure(runErr, len(output), parsed)
	if !fail {
		return nil
	}
	r.printStaticcheckFailure(parsed, runErr != nil, stderr)
	if runErr != nil {
		return fmt.Errorf("running staticcheck (exit %d): %w", exitCode, runErr)
	}
	return errors.New("staticcheck found SA issues on changed lines")
}

func (r *Runner) runStaticcheck(ctx context.Context, module string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, "staticcheck", "-checks", "SA*", "-f", "json", "./...")
	cmd.Dir = filepath.Join(r.Root, filepath.FromSlash(module))
	cmd.Env = os.Environ()
	for key, value := range r.Config.GoVet.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r.stopLocalCommand(cmd, true)
	err := cmd.Run()
	return bytes.Clone(stdout.Bytes()), stderr.String(), err
}

func parseStaticcheckOutput(output []byte, module, root string, changed map[string]map[int]bool) (parsedStaticcheck, error) {
	var parsed parsedStaticcheck
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		diagnostic, err := decodeStaticcheckDiagnostic(scanner.Bytes())
		if err != nil {
			return parsedStaticcheck{}, err
		}
		line, file, err := staticcheckDiagnosticLine(diagnostic, module, root)
		if err != nil {
			return parsedStaticcheck{}, err
		}
		parsed.diagnosticLines = append(parsed.diagnosticLines, line)
		if strings.HasPrefix(diagnostic.Code, "SA") {
			if changed[file][diagnostic.Location.Line] {
				parsed.findings = append(parsed.findings, line)
			}
		} else {
			parsed.nonSA = append(parsed.nonSA, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedStaticcheck{}, fmt.Errorf("reading staticcheck output: %w", err)
	}
	return parsed, nil
}

func decodeStaticcheckDiagnostic(data []byte) (staticcheckDiagnostic, error) {
	var diagnostic staticcheckDiagnostic
	if err := json.Unmarshal(data, &diagnostic); err != nil {
		return staticcheckDiagnostic{}, fmt.Errorf("reading staticcheck JSON: %w", err)
	}
	return diagnostic, nil
}

func staticcheckDiagnosticLine(d staticcheckDiagnostic, module, root string) (string, string, error) {
	file := filepath.ToSlash(d.Location.File)
	if d.Location.File != "" {
		if filepath.IsAbs(d.Location.File) {
			rel, err := filepath.Rel(root, d.Location.File)
			if err != nil {
				return "", "", fmt.Errorf("making staticcheck path relative to repository: %w", err)
			}
			file = filepath.ToSlash(rel)
		} else {
			file = path.Clean(path.Join(module, file))
		}
	}
	line := d.Code + ": " + d.Message
	if d.Location.File != "" && d.Location.Line > 0 {
		line = fmt.Sprintf("%s:%d: %s %s", file, d.Location.Line, d.Code, d.Message)
	}
	return line, file, nil
}

func staticcheckFailure(runErr error, outputBytes int, parsed parsedStaticcheck) (int, bool) {
	fail := len(parsed.findings) > 0
	if runErr == nil {
		return 0, fail
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		code = exitErr.ExitCode()
	}
	if code != 1 || outputBytes == 0 || len(parsed.diagnosticLines) == 0 || len(parsed.nonSA) > 0 {
		fail = true
	}
	return code, fail
}

func (r *Runner) printStaticcheckFailure(parsed parsedStaticcheck, commandFailed bool, stderr string) {
	lines := append([]string(nil), parsed.findings...)
	if commandFailed {
		lines = append(lines, parsed.nonSA...)
		if detail := strings.TrimSpace(stderr); detail != "" {
			lines = append(lines, detail)
		}
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	if len(lines) > 0 {
		fmt.Fprintln(r.Stdout, strings.Join(lines, "\n"))
	}
}
