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
	"os/user"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/execx"
	"github.com/aiseeq/graft/v2/internal/gitx"
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
	if err := checkOtherRemotes(repo, d.Remote, branch); err != nil {
		return "", err
	}
	return head, nil
}

// shownMissing is how many missing commits a refusal names.
const shownMissing = 5

// checkOtherRemotes fetches every other remote and refuses while one's
// branch has commits whose content the deployed remote's branch lacks: a
// pull request merged on a mirror would otherwise never go out, or go out
// later behind everyone's back. Content counts, not ancestry: a commit
// rebased or cherry-picked under another hash is there.
func checkOtherRemotes(repo *gitx.Repo, deployRemote, branch string) error {
	remotes, err := repo.Remotes()
	if err != nil {
		return err
	}
	base := deployRemote + "/" + branch
	for _, r := range remotes {
		if r == deployRemote {
			continue
		}
		ref := "refs/remotes/" + r + "/" + branch
		if _, err := repo.Git("ls-remote", "--exit-code", r, "refs/heads/"+branch); err != nil {
			if gitx.ExitCode(err) == 2 {
				if _, delErr := repo.Git("update-ref", "-d", ref); delErr != nil {
					return fmt.Errorf("removing stale remote-tracking ref %s: %w", ref, delErr)
				}
				continue
			}
			return fmt.Errorf("git fetch %s failed, cannot check that %s has everything %s/%s has (checking branch existence): %w", r, base, r, branch, err)
		}
		if _, err := repo.Git("fetch", "--quiet", "--prune", r, "+refs/heads/"+branch+":"+ref); err != nil {
			return fmt.Errorf("git fetch %s failed, cannot check that %s has everything %s/%s has: %w", r, base, r, branch, err)
		}
		other := r + "/" + branch
		exists, err := repo.RevExists(ref)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		out, err := repo.Git("rev-list", "--no-merges", "--reverse", ref, "^refs/remotes/"+base)
		if err != nil {
			return err
		}
		missing := make([]string, 0)
		for sha := range strings.FieldsSeq(out) {
			present, err := ChangePresent(repo, base, sha)
			if err != nil {
				return fmt.Errorf("checking the change in %s: %w", gitx.Short(sha), err)
			}
			if !present {
				missing = append(missing, sha)
			}
		}
		if len(missing) == 0 {
			continue
		}
		var lines []string
		for _, sha := range missing[:min(len(missing), shownMissing)] {
			subject, err := repo.Git("log", "-1", "--format=%s", sha)
			if err != nil {
				return err
			}
			lines = append(lines, gitx.Short(sha)+" "+strings.TrimSpace(subject))
		}
		if len(missing) > shownMissing {
			lines = append(lines, fmt.Sprintf("and %d more", len(missing)-shownMissing))
		}
		noun := "commits"
		if len(missing) == 1 {
			noun = "commit"
		}
		return fmt.Errorf("%s has %d %s that %s lacks: %s\nnothing was deployed; bring them here and publish them everywhere, then deploy:\n  git merge %s\n  git push %s %s   # and every other remote that lacks the merge",
			other, len(missing), noun, base, strings.Join(lines, "; "), other, deployRemote, branch)
	}
	return nil
}

// ChangePresent reports whether commit's exact changed lines are present on
// base. A temporary index lets the check inspect any commit without changing
// the work tree or the caller's index.
func ChangePresent(repo *gitx.Repo, base, commit string) (bool, error) {
	return ChangePresentExcept(repo, base, commit)
}

// ChangePresentExcept checks the commit's changed lines while ignoring the
// given paths, such as generated version files. A root commit's change is
// against the empty tree.
func ChangePresentExcept(repo *gitx.Repo, base, commit string, exclude ...string) (present bool, retErr error) {
	parent, err := repo.ParentTree(commit)
	if err != nil {
		return false, err
	}
	args := []string{"diff", "--binary", "--no-ext-diff", "--no-renames", "--unified=0", parent, commit, "--"}
	for _, path := range exclude {
		args = append(args, ":(exclude)"+path)
	}
	diff, err := repo.Git(args...)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(diff) == "" {
		return true, nil
	}
	index, err := os.CreateTemp("", "graft-deploy-index-")
	if err != nil {
		return false, fmt.Errorf("creating temporary git index: %w", err)
	}
	path := index.Name()
	if err := index.Close(); err != nil {
		removeErr := os.Remove(path)
		return false, errors.Join(fmt.Errorf("closing temporary git index: %w", err), removeErr)
	}
	defer func() {
		if err := os.Remove(path); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("removing temporary git index: %w", err))
		}
	}()
	env := []string{"GIT_INDEX_FILE=" + path, "LC_ALL=C"}
	if _, err := repo.GitEnv(env, "read-tree", base); err != nil {
		return false, err
	}
	for _, hunk := range diffHunks(diff) {
		present, err := hunkPresent(repo, env, hunk.patch, hunk.hasAddedLines)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

type diffHunk struct {
	patch         string
	hasAddedLines bool
}

// diffHunks keeps the file header with each hunk so git can check that hunk
// against the same base independently of other changes in the commit. A file
// with no line hunks, a binary or a mode-only change, is one patch whose
// reverse must apply to the base for the change to be present.
func diffHunks(diff string) []diffHunk {
	var hunks []diffHunk
	var header, current strings.Builder
	inFile, fileHunks, hasHunk, hasAddedLines := false, 0, false, false
	flushHunk := func() {
		if hasHunk {
			hunks = append(hunks, diffHunk{patch: current.String(), hasAddedLines: hasAddedLines})
			fileHunks++
			current.Reset()
			current.WriteString(header.String())
			hasHunk, hasAddedLines = false, false
		}
	}
	flushFile := func() {
		flushHunk()
		if inFile && fileHunks == 0 {
			hunks = append(hunks, diffHunk{patch: header.String(), hasAddedLines: true})
		}
	}
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			inFile, fileHunks = true, 0
			header.Reset()
			header.WriteString(line + "\n")
			current.Reset()
			current.WriteString(line + "\n")
		case strings.HasPrefix(line, "@@ "):
			flushHunk()
			current.WriteString(line + "\n")
			hasHunk = true
		case hasHunk:
			current.WriteString(line + "\n")
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				hasAddedLines = true
			}
		default:
			header.WriteString(line + "\n")
			current.WriteString(line + "\n")
		}
	}
	flushFile()
	return hunks
}

func hunkPresent(repo *gitx.Repo, env []string, patch string, hasAddedLines bool) (bool, error) {
	args := []string{"apply", "--cached", "--check", "--unidiff-zero"}
	if hasAddedLines {
		args = append(args, "--reverse")
	}
	if _, err := repo.GitInput(strings.NewReader(patch), env, args...); err == nil {
		return hasAddedLines, nil
	} else {
		var gitErr *gitx.Error
		if gitx.ExitCode(err) == 1 && errors.As(err, &gitErr) && expectedPatchMismatch(gitErr.Stderr) {
			return !hasAddedLines, nil
		}
		return false, err
	}
}

func expectedPatchMismatch(stderr string) bool {
	return strings.Contains(stderr, "patch failed") || strings.Contains(stderr, "patch does not apply") ||
		strings.Contains(stderr, "does not exist in index") || strings.Contains(stderr, "already exists in index")
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

// While another sudo holds the reader, sudo without a terminal does not get to
// ask it. fingerprintBusyWait bounds that run of attempts: pam_fprintd holds
// the reader for at most three tries of 30 seconds. fingerprintListWait bounds
// the question whether this user has a finger enrolled at all.
const (
	fingerprintBusyWait  = 2 * time.Minute
	fingerprintBusyPause = 3 * time.Second
	fingerprintListWait  = 5 * time.Second
)

// fingerprintNotAsked is what sudo prints, in the C locale, when PAM got past
// the reader to the password prompt. A reader another sudo holds and no reader
// at all print the same, so fingerprintEnrolled tells them apart.
const fingerprintNotAsked = "a terminal is required to read the password"

// fingerprintRefusals are what pam_fprintd prints, in the C locale, when it
// asked the reader and got no finger in time or one it does not know.
var fingerprintRefusals = []string{"Verification timed out", "Failed to match fingerprint"}

// ConfirmSudo confirms the deploy with sudo. It invalidates cached credentials
// and checks that non-interactive authentication does not silently succeed
// before asking for confirmation. In a terminal it asks until the password is
// given; Ctrl-C stops. Without a terminal it asks again while the fingerprint
// reader refuses, for at most fingerprintWait, and any other refusal ends the
// deploy, since nobody can type a password there.
func ConfirmSudo(ctx context.Context, target string, log io.Writer) error {
	confirmed, err := checkSudoConfirmation(ctx, target)
	if err != nil {
		return err
	}
	if confirmed {
		return nil
	}
	prompt := fmt.Sprintf("[sudo] password to confirm the deploy to %s: ", target)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return confirmInTerminal(ctx, prompt, log)
	}
	fmt.Fprintf(log, "graft: no terminal, touch the fingerprint reader to confirm the deploy to %s (waiting up to %s, Ctrl-C to stop)\n", target, fingerprintWait)
	limits := fingerprintLimits{wait: fingerprintWait, busyWait: fingerprintBusyWait, busyPause: fingerprintBusyPause, now: time.Now}
	return confirmByFingerprint(ctx, target, limits, log, fingerprintEnrolled, func(ctx context.Context) (bool, []byte, error) {
		cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "sudo", "-v", "-p", prompt)
		if err != nil {
			return false, nil, err
		}
		// The reader's messages are matched in English.
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.MultiWriter(os.Stdout, &out), io.MultiWriter(os.Stderr, &stderr)
		runErr := cmd.Run()
		return sudoResult(runErr, append(out.Bytes(), stderr.Bytes()...))
	})
}

// checkSudoConfirmation clears the current timestamp before the first attempt
// and detects a silent non-interactive success (for example, NOPASSWD).
func checkSudoConfirmation(ctx context.Context, target string) (bool, error) {
	invalidate, err := execx.CommandContext(ctx, execx.ShortGrace, "sudo", "-k")
	if err != nil {
		return false, fmt.Errorf("deploying to %s was not confirmed: sudo -k: %w", target, err)
	}
	if err := invalidate.Run(); err != nil {
		return false, fmt.Errorf("deploying to %s was not confirmed: sudo -k: %w", target, err)
	}

	check, err := execx.CommandContext(ctx, execx.ShortGrace, "sudo", "-n", "-v")
	if err != nil {
		return false, fmt.Errorf("deploying to %s was not confirmed: sudo -n -v: %w", target, err)
	}
	check.Env = append(os.Environ(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	check.Stdout = io.MultiWriter(os.Stdout, &stdout)
	check.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := check.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("deploying to %s was not confirmed: sudo -n -v: %w", target, err)
	}
	if fingerprintPrompted(append(stdout.Bytes(), stderr.Bytes()...)) {
		// pam_fprintd reports the requested finger through PAM before checking
		// it. A successful check after this prompt means the user confirmed.
		return true, nil
	}
	return false, fmt.Errorf("deploying to %s was not confirmed: sudo on this host does not confirm anything: sudo passes without a password or fingerprint; remove NOPASSWD for the user or remove confirm from the target", target)
}

func fingerprintPrompted(out []byte) bool {
	return (bytes.Contains(out, []byte("Place your ")) || bytes.Contains(out, []byte("Swipe your "))) &&
		bytes.Contains(out, []byte("finger"))
}

func confirmInTerminal(ctx context.Context, prompt string, log io.Writer) error {
	for {
		cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "sudo", "-v", "-p", prompt)
		if err != nil {
			return err
		}
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

// fingerprintLimits are the bounds of confirmByFingerprint: wait for the whole
// confirmation, busyWait for a run of attempts that did not reach the reader,
// busyPause between those attempts.
type fingerprintLimits struct {
	wait, busyWait, busyPause time.Duration
	now                       func() time.Time
}

// confirmByFingerprint repeats attempt while its output shows the fingerprint
// reader refusing or busy, until it passes, ctx ends or the limits run out.
// enrolled says why this user cannot be asked for a finger, nil when they can.
func confirmByFingerprint(ctx context.Context, target string, limits fingerprintLimits, log io.Writer, enrolled func(context.Context) error, attempt func(context.Context) (bool, []byte, error)) error {
	waitCtx, cancel := context.WithTimeout(ctx, limits.wait)
	defer cancel()
	deadline, _ := waitCtx.Deadline()
	stopped := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if waitCtx.Err() != nil {
			return fmt.Errorf("deploying to %s was not confirmed: no fingerprint within %s", target, limits.wait)
		}
		return nil
	}
	refuse := func(reason string) error {
		return fmt.Errorf("deploying to %s was not confirmed: without a terminal sudo takes only cached credentials or a fingerprint, and the fingerprint reader did not answer%s; run graft deploy in a terminal to type the password", target, reason)
	}
	var busySince time.Time
	for n := 1; ; n++ {
		ok, out, err := attempt(waitCtx)
		if ok {
			return nil
		}
		if err := stopped(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if fingerprintRefused(out) {
			busySince = time.Time{}
			fmt.Fprintf(log, "graft: fingerprint attempt %d refused, asking again (%s left, Ctrl-C to stop)\n", n, time.Until(deadline).Round(time.Second))
			continue
		}
		if !bytes.Contains(out, []byte(fingerprintNotAsked)) {
			return refuse("")
		}
		enrolledErr := enrolled(waitCtx)
		if err := stopped(); err != nil {
			return err
		}
		if enrolledErr != nil {
			return refuse(fmt.Sprintf(" (%v)", enrolledErr))
		}
		if busySince.IsZero() {
			busySince = limits.now()
		}
		if limits.now().Sub(busySince) >= limits.busyWait {
			return refuse(fmt.Sprintf(" (busy for %s)", limits.busyWait))
		}
		fmt.Fprintf(log, "graft: the fingerprint reader looks busy (attempt %d), asking again in %s (%s left, Ctrl-C to stop)\n", n, limits.busyPause, time.Until(deadline).Round(time.Second))
		select {
		case <-waitCtx.Done():
			return stopped()
		case <-time.After(limits.busyPause):
		}
	}
}

// fingerprintEnrolled asks fprintd whether the user sudo authenticates has a
// finger enrolled on a reader; the error says why not.
func fingerprintEnrolled(ctx context.Context) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("finding the current user: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fingerprintListWait)
	defer cancel()
	cmd, err := execx.CommandContext(ctx, execx.ShortGrace, "fprintd-list", u.Username)
	if err != nil {
		return err
	}
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return enrolledFingers(out, err)
}

// enrolledFingers reads what fprintd-list printed: a line per enrolled finger.
func enrolledFingers(out []byte, runErr error) error {
	if runErr != nil {
		return fmt.Errorf("fprintd-list: %w: %s", runErr, strings.TrimSpace(string(out)))
	}
	if !bytes.Contains(out, []byte(" - #")) {
		return errors.New("fprintd-list shows no enrolled finger")
	}
	return nil
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
