// Package deploy holds the steps graft wraps around a project's deploy
// script: the published-HEAD check, the production confirmation, the deployed
// commit record and the release notes.
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

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
		return "", fmt.Errorf("HEAD %s is not %s/%s %s: push local commits or pull (git pull --ff-only)", gitx.Short(head), d.Remote, branch, gitx.Short(remote))
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
		return fmt.Errorf("HEAD moved during the deploy (%s -> %s): what was built is not the checked commit", gitx.Short(want), gitx.Short(head))
	}
	return nil
}

// fingerprintWait bounds how long a deploy without a terminal waits for a
// finger on the reader.
const fingerprintWait = 10 * time.Minute

// fingerprintRefusals are what pam_fprintd prints, in the C locale, when it
// asked the reader and got no finger in time or one it does not know.
var fingerprintRefusals = []string{"Verification timed out", "Failed to match fingerprint"}

// ConfirmSudo confirms the deploy with sudo. In a terminal it asks until the
// password is given; Ctrl-C stops. Without a terminal sudo passes only on
// credentials it has cached or on a PAM method that reads nothing from stdin
// (a fingerprint reader): it asks again while the reader refuses, for at most
// fingerprintWait, and any other refusal ends the deploy, since nobody can
// type a password there.
func ConfirmSudo(ctx context.Context, target string, log io.Writer) error {
	prompt := fmt.Sprintf("[sudo] password to confirm the deploy to %s: ", target)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return confirmInTerminal(ctx, prompt, log)
	}
	fmt.Fprintf(log, "graft: no terminal, touch the fingerprint reader to confirm the deploy to %s (waiting up to %s, Ctrl-C to stop)\n", target, fingerprintWait)
	return confirmByFingerprint(ctx, target, fingerprintWait, log, func(ctx context.Context) (bool, []byte, error) {
		cmd := exec.CommandContext(ctx, "sudo", "-v", "-p", prompt)
		// The reader's messages are matched in English.
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &out), os.Stderr
		return sudoResult(cmd.Run(), out.Bytes())
	})
}

func confirmInTerminal(ctx context.Context, prompt string, log io.Writer) error {
	for {
		cmd := exec.CommandContext(ctx, "sudo", "-v", "-p", prompt)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		ok, _, err := sudoResult(cmd.Run(), nil)
		if ok {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		fmt.Fprintln(log, "graft: not confirmed, asking again (Ctrl-C to stop)")
	}
}

// sudoResult tells a sudo that refused (false, nil) from one that did not run.
func sudoResult(runErr error, out []byte) (bool, []byte, error) {
	if runErr == nil {
		return true, out, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return false, out, fmt.Errorf("sudo: %w", runErr)
	}
	return false, out, nil
}

// confirmByFingerprint repeats attempt while its output shows the fingerprint
// reader refusing, until it passes, ctx ends or wait runs out.
func confirmByFingerprint(ctx context.Context, target string, wait time.Duration, log io.Writer, attempt func(context.Context) (bool, []byte, error)) error {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	for n := 1; ; n++ {
		ok, out, err := attempt(waitCtx)
		if ok {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if waitCtx.Err() != nil {
			return fmt.Errorf("deploying to %s was not confirmed: no fingerprint within %s", target, wait)
		}
		if err != nil {
			return err
		}
		if !fingerprintRefused(out) {
			return fmt.Errorf("deploying to %s was not confirmed: without a terminal sudo takes only cached credentials or a fingerprint, and the fingerprint reader did not answer; run graft deploy in a terminal to type the password", target)
		}
		fmt.Fprintf(log, "graft: fingerprint attempt %d refused, asking again (%s left, Ctrl-C to stop)\n", n, time.Until(deadline).Round(time.Second))
	}
}

func fingerprintRefused(out []byte) bool {
	for _, m := range fingerprintRefusals {
		if bytes.Contains(out, []byte(m)) {
			return true
		}
	}
	return false
}

// ReadDeployedSHA returns the commit recorded in the environment, or "" when
// nothing was recorded yet.
func ReadDeployedSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) (string, error) {
	return readDeployedSHA(ctx, env, rec, sudo(rec))
}

// PeekDeployedSHA reads the deployed commit without ever asking for a sudo
// password (sudo -n): for status, which must not wait on a prompt.
func PeekDeployedSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA) (string, error) {
	prefix := ""
	if rec.Sudo {
		prefix = "sudo -n "
	}
	return readDeployedSHA(ctx, env, rec, prefix)
}

func readDeployedSHA(ctx context.Context, env *envs.Env, rec *config.DeployedSHA, sudoPrefix string) (string, error) {
	// The existence check runs under sudo too: a file in a directory only
	// root can enter looks absent to the deploy user.
	read := "if [ -e " + envs.Quote(rec.Path) + " ]; then cat " + envs.Quote(rec.Path) + "; fi"
	line := read
	if sudoPrefix != "" {
		line = sudoPrefix + "sh -c " + envs.Quote(read)
	}
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
