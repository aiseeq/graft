// Package tools checks and installs the project's pinned development tools.
// A tool counts as present only when its check command prints what the
// config expects: a binary on PATH says nothing about its version or rules.
package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
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

// Check runs the check of one tool.
func Check(ctx context.Context, name string, t *config.Tool) (Result, error) {
	r := Result{Name: name}
	path, err := exec.LookPath(t.Check[0])
	if errors.Is(err, exec.ErrNotFound) {
		r.Problem = t.Check[0] + " is not on PATH"
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.Path = path
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

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" {
		return "(no output)"
	}
	return line
}

// Install runs go install for one tool and checks it again. When the check
// still fails because PATH finds another copy first, the error says so.
func Install(ctx context.Context, name string, t *config.Tool, log io.Writer) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("tools are installed with go install: %w", err)
	}
	args := []string{"install"}
	if len(t.Tags) > 0 {
		args = append(args, "-tags", strings.Join(t.Tags, ","))
	}
	args = append(args, t.GoInstall)
	fmt.Fprintf(log, "graft: go %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "go", args...)
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
	binDir, err := goBin(ctx)
	if err != nil {
		return err
	}
	if r.Path != "" && filepath.Dir(r.Path) != binDir {
		return fmt.Errorf("%s: installed into %s, but PATH finds %s first: %s", name, binDir, r.Path, r.Problem)
	}
	if r.Path == "" {
		return fmt.Errorf("%s: installed into %s, which is not on PATH", name, binDir)
	}
	return fmt.Errorf("%s: still failing after install: %s", name, r.Problem)
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
