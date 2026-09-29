// Package deploy holds the steps graft wraps around a project's deploy
// script: the published-HEAD check, the production confirmation, the deployed
// commit record and the release notes.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/gitx"
)

// CheckTree refuses a work tree whose content is not exactly a commit: a
// deploy built from it would not match the commit it is recorded under.
func CheckTree(repo *gitx.Repo, d *config.Deploy) error {
	if len(d.Untracked) == 0 {
		out, err := repo.Git("status", "--porcelain")
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(out); s != "" {
			return fmt.Errorf("the work tree is not clean, only committed code is deployed:\n%s", s)
		}
		return nil
	}
	out, err := repo.Git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("uncommitted changes, only committed code is deployed:\n%s", s)
	}
	out, err = repo.Git(append([]string{"ls-files", "--others", "--exclude-standard", "--"}, d.Untracked...)...)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(out); s != "" {
		return fmt.Errorf("untracked files would go into the build but not into git; commit or delete them:\n%s", s)
	}
	return nil
}

// PublishedHead fetches the remote and returns HEAD, provided it is exactly
// the remote branch: a deploy ships only what everyone can see.
func PublishedHead(repo *gitx.Repo, d *config.Deploy) (string, error) {
	branch := d.Branch
	if branch == "" {
		b, err := repo.Branch()
		if err != nil {
			return "", err
		}
		branch = b
	}
	if _, err := repo.Git("fetch", "--quiet", d.Remote); err != nil {
		return "", fmt.Errorf("git fetch %s failed, cannot check that HEAD is published: %w", d.Remote, err)
	}
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	ref := "refs/remotes/" + d.Remote + "/" + branch
	remote, err := repo.Git("rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", fmt.Errorf("%s/%s does not exist: publish the branch first (graft commit pushes it)", d.Remote, branch)
	}
	if remote = strings.TrimSpace(remote); remote != head {
		return "", fmt.Errorf("HEAD %s is not %s/%s %s: push local commits or pull (git pull --ff-only)", head[:12], d.Remote, branch, remote[:min(12, len(remote))])
	}
	return head, nil
}

// CheckHead verifies, mid-deploy, that the tree is still clean and HEAD is
// still the commit the deploy started from.
func CheckHead(repo *gitx.Repo, d *config.Deploy, want string) error {
	if err := CheckTree(repo, d); err != nil {
		return err
	}
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head = strings.TrimSpace(head); head != want {
		return fmt.Errorf("HEAD moved during the deploy (%s -> %s): what was built is not the checked commit", want[:12], head[:12])
	}
	return nil
}

// RequireTerminal refuses to go on without an interactive stdin: a sudo
// prompt in a background job waits forever where nobody sees it.
func RequireTerminal(target string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("deploying to %s asks for confirmation: run graft deploy in the foreground of a terminal", target)
	}
	return nil
}

// ConfirmSudo asks for the sudo password until it is given; Ctrl-C stops.
func ConfirmSudo(ctx context.Context, target string, log io.Writer) error {
	prompt := fmt.Sprintf("[sudo] password to confirm the deploy to %s: ", target)
	for {
		cmd := exec.CommandContext(ctx, "sudo", "-v", "-p", prompt)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return fmt.Errorf("sudo: %w", err)
		}
		fmt.Fprintln(log, "graft: not confirmed, asking again (Ctrl-C to stop)")
	}
}

// ReadDeployedSHA returns the commit recorded in the environment, or "" when
// nothing was recorded yet.
func ReadDeployedSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) (string, error) {
	line := "if [ -e " + envs.Quote(rec.Path) + " ]; then " + sudo(rec) + "cat " + envs.Quote(rec.Path) + "; fi"
	cmd := env.Shell(ctx, line)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading %s on %s: %w: %s", rec.Path, env.Name, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteDeployedSHA records sha in the environment.
func WriteDeployedSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA, sha string) error {
	cmd := env.Shell(ctx, sudo(rec)+"tee "+envs.Quote(rec.Path)+" >/dev/null")
	cmd.Stdin = strings.NewReader(sha + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("writing %s on %s: %w: %s", rec.Path, env.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sudo(rec *config.DeployedSHA) string {
	if rec.Sudo {
		return "sudo "
	}
	return ""
}

// DeployedVersion runs the target's version command and returns its trimmed
// output.
func DeployedVersion(ctx context.Context, env *envs.Env, c config.EnvCommand) (string, error) {
	cmd, err := env.Command(ctx, c)
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading the deployed version on %s: %w: %s", env.Name, err, strings.TrimSpace(stderr.String()))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", fmt.Errorf("the version command on %s printed nothing", env.Name)
	}
	return v, nil
}
