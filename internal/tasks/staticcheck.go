package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type staticcheckPackage struct {
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Module       *struct {
		Main bool
	}
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
	output, stderr, runErr := r.runStaticcheck(ctx, module, true)
	dir := filepath.Join(r.Root, filepath.FromSlash(module))
	var externalPaths []string
	var parsed parsedStaticcheck
	var untranslated bool
	var files map[string]bool
	if len(changed) > 0 {
		foreignAbsolute, foreignErr := staticcheckHasForeignAbsolutePath(output, module, r.Root)
		if foreignErr != nil {
			return foreignErr
		}
		if foreignAbsolute {
			externalPaths, err = r.staticcheckExternalPaths(ctx, dir)
		}
		if err != nil {
			return staticcheckWithListingError(err, runErr, stderr)
		}
		parsed, untranslated, err = parseStaticcheckOutput(output, module, r.Root, nil, changed, externalPaths)
		if err != nil {
			return err
		}
		if untranslated {
			files, err = r.staticcheckModuleFiles(ctx, module)
			if err != nil {
				return staticcheckWithListingError(err, runErr, stderr)
			}
			parsed, untranslated, err = parseStaticcheckOutput(output, module, r.Root, files, changed, externalPaths)
			if err != nil {
				return err
			}
		}
	} else {
		parsed, _, err = parseStaticcheckOutput(output, module, r.Root, nil, changed, nil)
		if err != nil {
			return err
		}
	}
	if untranslated {
		noticeWriter := r.Stderr
		if noticeWriter == nil {
			noticeWriter = os.Stderr
		}
		fmt.Fprintln(noticeWriter, staticcheckRerunNotice)
		output, stderr, runErr = r.runStaticcheck(ctx, module, false)
		parsed, untranslated, err = parseStaticcheckOutput(output, module, r.Root, files, changed, externalPaths)
		if err != nil {
			return err
		}
		if untranslated {
			writer := r.Stderr
			if writer == nil {
				writer = os.Stderr
			}
			fmt.Fprintln(writer, staticcheckRerunUntranslatedError)
			return errors.New(staticcheckRerunUntranslatedError)
		}
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

func staticcheckHasForeignAbsolutePath(output []byte, module, root string) (bool, error) {
	moduleDir := filepath.Join(root, filepath.FromSlash(module))
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		diagnostic, err := decodeStaticcheckDiagnostic(scanner.Bytes())
		if err != nil {
			return false, err
		}
		if !filepath.IsAbs(diagnostic.Location.File) {
			continue
		}
		if _, own, err := staticcheckLocalModuleFile(diagnostic.Location.File, moduleDir); err != nil {
			return false, err
		} else if !own {
			return true, nil
		}
	}
	return false, scanner.Err()
}

func staticcheckWithListingError(listErr, runErr error, stderr string) error {
	if runErr == nil {
		return listErr
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("%w (staticcheck also failed: %v; stderr: %s)", listErr, runErr, detail)
	}
	return fmt.Errorf("%w (staticcheck also failed: %v)", listErr, runErr)
}

const staticcheckRerunNotice = "graft: staticcheck reported a path outside this module's file list; rerun without -trimpath for this tree's paths"
const staticcheckRerunUntranslatedError = "staticcheck still reported an untranslated path after rerunning with -trimpath=false"

func (r *Runner) runStaticcheck(ctx context.Context, module string, trim bool) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, "staticcheck", "-checks", "SA*", "-f", "json", "./...")
	cmd.Dir = filepath.Join(r.Root, filepath.FromSlash(module))
	cmd.Env = os.Environ()
	for key, value := range r.Config.GoVet.Env {
		cmd.Env = setEnv(cmd.Env, key, value)
	}
	if trim {
		// GOFLAGS as go resolves it, env or GOENV file: setting it in env
		// replaces the file's value, so the resolved text is what goes on.
		flagsText, err := resolveGoFlags(ctx, cmd.Dir, cmd.Env)
		if err != nil {
			return nil, "", fmt.Errorf("staticcheck -trimpath: reading GOFLAGS: %w", err)
		}
		flags, err := splitGoFlags(flagsText)
		if err != nil {
			return nil, "", fmt.Errorf("staticcheck -trimpath: parsing GOFLAGS: %w", err)
		}
		if argv := addTrimPath([]string{"go", "vet"}, flags, []string{"vet"}); len(argv) > 2 {
			cmd.Env = setEnv(cmd.Env, "GOFLAGS", strings.TrimSpace(flagsText+" -trimpath"))
		}
	} else {
		flagsText, err := resolveGoFlags(ctx, cmd.Dir, cmd.Env)
		if err != nil {
			return nil, "", fmt.Errorf("staticcheck retry: reading GOFLAGS: %w", err)
		}
		cmd.Env = setEnv(cmd.Env, "GOFLAGS", strings.TrimSpace(flagsText+" -trimpath=false"))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r.stopLocalCommand(cmd, true)
	err := cmd.Run()
	return bytes.Clone(stdout.Bytes()), stderr.String(), err
}

func setEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, item := range env {
		name, _, ok := strings.Cut(item, "=")
		if !ok || !strings.EqualFold(name, key) {
			result = append(result, item)
		}
	}
	return append(result, key+"="+value)
}

func (r *Runner) staticcheckModuleFiles(ctx context.Context, module string) (map[string]bool, error) {
	dir := filepath.Join(r.Root, filepath.FromSlash(module))
	env := os.Environ()
	for key, value := range r.Config.GoVet.Env {
		env = setEnv(env, key, value)
	}
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-test", "-json", "./...")
	cmd.Dir, cmd.Env = dir, env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r.stopLocalCommand(cmd, true)
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("listing staticcheck module files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	files := make(map[string]bool)
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var pkg staticcheckPackage
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("reading staticcheck module file list: %w", err)
		}
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		for _, name := range slices.Concat(pkg.GoFiles, pkg.CgoFiles, pkg.TestGoFiles, pkg.XTestGoFiles) {
			full := filepath.Clean(filepath.Join(pkg.Dir, name))
			rel, err := filepath.Rel(dir, full)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				files[filepath.ToSlash(rel)] = true
			}
		}
	}
	return files, nil
}

func (r *Runner) staticcheckExternalPaths(ctx context.Context, dir string) ([]string, error) {
	env := os.Environ()
	for key, value := range r.Config.GoVet.Env {
		env = setEnv(env, key, value)
	}
	out, err := goEnvOutput(ctx, dir, env, "GOROOT", "GOMODCACHE")
	if err != nil {
		return nil, fmt.Errorf("getting go env GOROOT and GOMODCACHE for staticcheck: %w", err)
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) != 2 {
		return nil, fmt.Errorf("getting go env GOROOT and GOMODCACHE for staticcheck: expected two lines, got %q", out)
	}
	return values, nil
}

func parseStaticcheckOutput(output []byte, module, root string, moduleFiles map[string]bool, changed map[string]map[int]bool, externalPaths []string) (parsedStaticcheck, bool, error) {
	var parsed parsedStaticcheck
	untranslated := false
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		diagnostic, err := decodeStaticcheckDiagnostic(scanner.Bytes())
		if err != nil {
			return parsedStaticcheck{}, false, err
		}
		line, file, own, candidates, err := staticcheckDiagnosticLine(diagnostic, module, root, moduleFiles, externalPaths)
		if err != nil {
			return parsedStaticcheck{}, false, err
		}
		if diagnostic.Location.File != "" && !own && !isExternalGoPath(diagnostic.Location.File, externalPaths) {
			untranslated = true
		}
		parsed.diagnosticLines = append(parsed.diagnosticLines, line)
		if strings.HasPrefix(diagnostic.Code, "SA") {
			changedLine := own && changed[file][diagnostic.Location.Line]
			for _, candidate := range candidates {
				changedLine = changedLine || changed[candidate][diagnostic.Location.Line]
			}
			if changedLine {
				parsed.findings = append(parsed.findings, line)
			}
		} else {
			parsed.nonSA = append(parsed.nonSA, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedStaticcheck{}, false, fmt.Errorf("reading staticcheck output: %w", err)
	}
	return parsed, untranslated, nil
}

func decodeStaticcheckDiagnostic(data []byte) (staticcheckDiagnostic, error) {
	var diagnostic staticcheckDiagnostic
	if err := json.Unmarshal(data, &diagnostic); err != nil {
		return staticcheckDiagnostic{}, fmt.Errorf("reading staticcheck JSON: %w", err)
	}
	return diagnostic, nil
}

func staticcheckDiagnosticLine(d staticcheckDiagnostic, module, root string, moduleFiles map[string]bool, externalPaths []string) (string, string, bool, []string, error) {
	file := filepath.ToSlash(d.Location.File)
	own := false
	var candidates []string
	if d.Location.File != "" {
		moduleDir := filepath.Join(root, filepath.FromSlash(module))
		if rel, isModuleFile, err := staticcheckLocalModuleFile(d.Location.File, moduleDir); err != nil {
			return "", "", false, nil, err
		} else if isModuleFile {
			file, own = path.Clean(path.Join(module, filepath.ToSlash(rel))), true
			candidates = []string{file}
		} else if len(moduleFiles) > 0 && !isExternalGoPath(d.Location.File, externalPaths) {
			matches := staticcheckPathCandidates(d.Location.File, module, moduleFiles, externalPaths)
			file, own = translatedStaticcheckFile(d.Location.File, module, matches)
			candidates = staticcheckModulePaths(module, matches)
		} else {
			file = filepath.ToSlash(filepath.Clean(filepath.FromSlash(file)))
		}
	}
	line := d.Code + ": " + d.Message
	if d.Location.File != "" && d.Location.Line > 0 {
		line = fmt.Sprintf("%s:%d: %s %s", file, d.Location.Line, d.Code, d.Message)
	}
	return line, file, own, candidates, nil
}

func staticcheckLocalModuleFile(name, moduleDir string) (string, bool, error) {
	rel := name
	if filepath.IsAbs(name) {
		var err error
		rel, err = filepath.Rel(moduleDir, name)
		if err != nil {
			return "", false, fmt.Errorf("relativizing staticcheck path %q to module directory %s: %w", name, moduleDir, err)
		}
	}
	rel = filepath.Clean(rel)
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !strings.HasSuffix(rel, ".go") {
		return "", false, nil
	}
	dirs := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
	dir := moduleDir
	for _, part := range dirs {
		if part == "." || part == "" {
			continue
		}
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") || part == "testdata" {
			return "", false, nil
		}
		dir = filepath.Join(dir, part)
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return "", false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("checking for nested go.mod in %s: %w", dir, err)
		}
	}
	return rel, true, nil
}

func translatedStaticcheckFile(diagnosticPath, module string, candidates []string) (string, bool) {
	if len(candidates) == 0 {
		return filepath.ToSlash(diagnosticPath), false
	}
	return path.Clean(path.Join(module, candidates[0])), true
}

func staticcheckModulePaths(module string, files []string) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, path.Clean(path.Join(module, file)))
	}
	return paths
}

func staticcheckPathCandidates(diagnosticPath, module string, moduleFiles map[string]bool, externalPaths []string) []string {
	if isExternalGoPath(diagnosticPath, externalPaths) {
		return nil
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(diagnosticPath)))
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	best := 0
	var matches []string
	for candidate := range moduleFiles {
		candidateParts := strings.Split(candidate, "/")
		common := 0
		for i, j := len(parts)-1, len(candidateParts)-1; i >= 0 && j >= 0 && parts[i] == candidateParts[j]; i, j = i-1, j-1 {
			common++
		}
		if !strings.Contains(candidate, "/") {
			rootSuffix := path.Join(module, candidate)
			if module == "." {
				rootSuffix = candidate
			}
			if pathHasSuffix(clean, rootSuffix) {
				common = len(strings.Split(rootSuffix, "/"))
			} else {
				continue
			}
		} else if common < 2 {
			continue
		}
		if common > best {
			best, matches = common, []string{candidate}
		} else if common > 0 && common == best {
			matches = append(matches, candidate)
		}
	}
	slices.Sort(matches)
	return matches
}

func pathHasSuffix(name, suffix string) bool {
	name = "/" + strings.Trim(name, "/") + "/"
	suffix = "/" + strings.Trim(suffix, "/") + "/"
	return strings.HasSuffix(name, suffix)
}

func isExternalGoPath(name string, prefixes []string) bool {
	if !filepath.IsAbs(name) {
		return false
	}
	clean := filepath.Clean(name)
	for _, prefix := range prefixes {
		if prefix != "" && (clean == prefix || strings.HasPrefix(clean, prefix+string(filepath.Separator))) {
			return true
		}
	}
	return false
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
