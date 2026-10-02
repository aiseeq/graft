// Package tools checks and installs the project's pinned development tools.
// A tool counts as present only when its check command prints what the
// config expects, or, for a go_install tool without one, when its Go build
// info matches the pin: a binary on PATH says nothing about its version.
package tools

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
)

// checkTimeout bounds one check command: a tool that hangs on --version
// fails its check.
const checkTimeout = 30 * time.Second

// Result is the outcome of one check.
type Result struct {
	Name string
	OK   bool
	// Problem says what is wrong when OK is false.
	Problem string
	// Path is where PATH finds the checked program, empty when it does not.
	Path string
}

// Check runs the check of one tool: its check command, or without one, a
// look at the Go build info of the program.
func Check(ctx context.Context, name string, t *config.Tool) (Result, error) {
	r := Result{Name: name}
	program := t.Program()
	path, err := exec.LookPath(program)
	if errors.Is(err, exec.ErrNotFound) {
		r.Problem = program + " is not on PATH"
		return r, nil
	}
	if err != nil {
		return r, fmt.Errorf("%s: looking up %s: %w", name, program, err)
	}
	r.Path = path
	if t.UsesBuildInfo() {
		bi, err := buildinfo.ReadFile(path)
		if err != nil {
			r.Problem = fmt.Sprintf("%s has no Go build info (%v)", path, err)
		} else {
			r.Problem = buildProblem(bi, t)
		}
		r.OK = r.Problem == ""
		return r, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, path, t.Check[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	switch {
	case runErr != nil:
		r.Problem = fmt.Sprintf("%s failed: %v: %s", strings.Join(t.Check, " "), runErr, firstLine(out.String()))
	case !strings.Contains(out.String(), t.Expect):
		r.Problem = fmt.Sprintf("%s does not print %q: %s", strings.Join(t.Check, " "), t.Expect, firstLine(out.String()))
	default:
		r.OK = true
	}
	return r, nil
}

// buildProblem compares a program's build info with the tool's pin: the
// main package, the main module version and, when the tool sets tags, the
// build tags. It returns what differs, empty when nothing does.
func buildProblem(bi *debug.BuildInfo, t *config.Tool) string {
	pkg, version := t.Pin()
	built := bi.Path + "@" + bi.Main.Version
	switch {
	case bi.Path != pkg:
		return fmt.Sprintf("built from %s, pinned %s", built, t.GoInstall)
	case bi.Main.Version != version:
		return fmt.Sprintf("built from %s, pinned %s", built, version)
	case len(t.Tags) == 0:
		return ""
	}
	want := sortedTags(t.Tags)
	var got []string
	for _, s := range bi.Settings {
		if s.Key == "-tags" {
			got = sortedTags(strings.Split(s.Value, ","))
		}
	}
	switch {
	case len(got) == 0:
		return "built without tags " + strings.Join(want, ",")
	case !slices.Equal(got, want):
		return fmt.Sprintf("built with tags %s, pinned %s", strings.Join(got, ","), strings.Join(want, ","))
	}
	return ""
}

// sortedTags is a tag list sorted, without empty and repeated tags.
func sortedTags(tags []string) []string {
	out := slices.DeleteFunc(slices.Clone(tags), func(s string) bool { return s == "" })
	slices.Sort(out)
	return slices.Compact(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" {
		return "(no output)"
	}
	return line
}

// Install runs go install for one tool into binDir (empty: where go install
// puts binaries) and checks it again. It warns before installing a copy that
// PATH will find ahead of another one, and fails when the check still finds
// another copy first.
func Install(ctx context.Context, name string, t *config.Tool, binDir string, log io.Writer) error {
	if t.GoInstall == "" {
		return fmt.Errorf("%s: not installed with go install; install it yourself: %s", name, t.Manual)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("tools are installed with go install: %w", err)
	}
	if binDir == "" {
		dir, err := goBin(ctx)
		if err != nil {
			return err
		}
		binDir = dir
	}
	shadowed, err := shadows(binDir, filepath.Base(t.Program()))
	if err != nil {
		return err
	}
	if len(shadowed) > 0 {
		fmt.Fprintf(log, "graft: warning: %s: installing into %s puts it on PATH ahead of %s, which will no longer run; set tools.bin_dir in the graft user config to install elsewhere\n",
			name, binDir, strings.Join(shadowed, ", "))
	}
	args := []string{"install"}
	if len(t.Tags) > 0 {
		args = append(args, "-tags", strings.Join(t.Tags, ","))
	}
	args = append(args, t.GoInstall)
	fmt.Fprintf(log, "graft: go %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = append(os.Environ(), "GOBIN="+binDir)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: go install: %w", name, err)
	}
	r, err := Check(ctx, name, t)
	if err != nil {
		return err
	}
	if r.OK {
		return nil
	}
	if r.Path != "" && filepath.Dir(r.Path) != binDir {
		return fmt.Errorf("%s: installed into %s, but PATH finds %s first: %s", name, binDir, r.Path, r.Problem)
	}
	if r.Path == "" {
		return fmt.Errorf("%s: installed into %s, which is not on PATH", name, binDir)
	}
	return fmt.Errorf("%s: still failing after install: %s", name, r.Problem)
}

// shadows lists the copies of program that PATH finds in directories after
// dir, which a copy installed into dir would hide.
func shadows(dir, program string) ([]string, error) {
	var found []string
	seen := false
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" {
			continue
		}
		if filepath.Clean(d) == filepath.Clean(dir) {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		p, err := exec.LookPath(filepath.Join(d, program))
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			continue // no copy there that runs
		case err != nil:
			return nil, fmt.Errorf("looking for %s in %s: %w", program, d, err)
		}
		found = append(found, p)
	}
	return found, nil
}

// goBin is where go install puts binaries.
func goBin(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "go", "env", "GOBIN", "GOPATH").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	gobin, gopath, _ := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\n")
	gobin, gopath = strings.TrimSpace(gobin), strings.TrimSpace(gopath)
	if gobin != "" {
		return gobin, nil
	}
	first, _, _ := strings.Cut(gopath, string(filepath.ListSeparator))
	if first == "" {
		return "", errors.New("go env reports neither GOBIN nor GOPATH")
	}
	return filepath.Join(first, "bin"), nil
}
