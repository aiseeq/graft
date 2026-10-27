package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// glintCheck counts, once a run, the glint findings release_notes.glint
// asks about in the deployed commit: with any, the items do not move unless
// ticket, graft deploy --glint-ticket, takes them.
type glintCheck struct {
	repo *gitx.Repo
	// severity is release_notes.glint, "" for no check; sha reads the
	// deployed commit.
	severity string
	sha      func(ctx context.Context) (string, error)
	ticket   string
	ran      bool
	// commit is the commit checked; found its findings, or err why they are
	// unknown.
	commit string
	found  int
	err    error
	// taken is set once the ticket is read: an open item of the project.
	taken     bool
	ticketErr error
}

// glintReport is the part of glint check -o json graft reads.
type glintReport struct {
	Summary struct {
		Total *int `json:"total"`
	} `json:"summary"`
	Stats struct {
		RulesPanicked []string `json:"rulesPanicked"`
	} `json:"stats"`
}

// held says why the items may not move, "" when they may: no findings, or
// the ticket takes them.
func (g *glintCheck) held(ctx context.Context, client *jira.Client) string {
	if g.severity == "" {
		return ""
	}
	if !g.ran {
		g.ran = true
		g.commit, g.found, g.err = g.count(ctx)
	}
	switch {
	case g.err != nil:
		return fmt.Sprintf("glint: %v (release_notes.glint)", g.err)
	case g.found == 0:
		return ""
	case g.ticket == "":
		return fmt.Sprintf("glint: %d findings of %s and above in %s (release_notes.glint)", g.found, g.severity, gitx.Short(g.commit))
	}
	if !g.taken && g.ticketErr == nil {
		g.ticketErr = g.readTicket(ctx, client)
		g.taken = g.ticketErr == nil
	}
	if g.ticketErr != nil {
		return fmt.Sprintf("glint: %d findings of %s and above in %s, and --glint-ticket %v", g.found, g.severity, gitx.Short(g.commit), g.ticketErr)
	}
	return ""
}

// takenBy says, for an item that moves with findings, which ticket takes
// them; "" when there are none.
func (g *glintCheck) takenBy() string {
	if !g.taken || g.found == 0 {
		return ""
	}
	return fmt.Sprintf(" with %d glint findings in %s: %s takes them (--glint-ticket)", g.found, gitx.Short(g.commit), g.ticket)
}

// newGlintCheck is the check of the target's release_notes.glint, one that
// holds nothing without it; a ticket needs it and must be an item of the
// project.
func newGlintCheck(r *deployRun, ticket string, sha func(ctx context.Context) (string, error)) (*glintCheck, error) {
	g := &glintCheck{repo: r.repo, sha: sha, ticket: ticket}
	if n := r.target.ReleaseNotes; n != nil {
		g.severity = n.Glint
	}
	switch {
	case ticket == "":
	case g.severity == "":
		return nil, fmt.Errorf("--glint-ticket: deploy.targets.%s.release_notes.glint is not set", r.name)
	case projectKeyRe(r.cfg).FindString(ticket) != ticket:
		return nil, fmt.Errorf("--glint-ticket: %q is not an item of release_notes.jira.project_keys, such as PROJ-123", ticket)
	}
	return g, nil
}

// checkTicket reads the ticket before a deploy, which a ticket that cannot
// take the findings then refuses: after the deploy it would only hold the
// items with a line.
func (g *glintCheck) checkTicket(ctx context.Context) error {
	if g.ticket == "" {
		return nil
	}
	client, err := jiraClient()
	if err != nil {
		return fmt.Errorf("--glint-ticket: %w", err)
	}
	if err := g.readTicket(ctx, client); err != nil {
		return fmt.Errorf("--glint-ticket %w", err)
	}
	g.taken = true
	return nil
}

// readTicket checks that the ticket is an item still to be done.
func (g *glintCheck) readTicket(ctx context.Context, client *jira.Client) error {
	issue, err := client.Issue(ctx, g.ticket)
	if err != nil {
		return fmt.Errorf("%s: %w", g.ticket, err)
	}
	if issue.Done {
		return fmt.Errorf("%s is %s: the findings need an item still to be done", g.ticket, issue.Status)
	}
	return nil
}

// count runs glint on the deployed commit: in the work tree when it is that
// commit and clean, else in a work tree of its own.
func (g *glintCheck) count(ctx context.Context) (sha string, found int, err error) {
	if sha, err = g.sha(ctx); err != nil {
		return "", 0, fmt.Errorf("the deployed commit is unknown: %w", err)
	}
	if sha == "" {
		return "", 0, errors.New("the deployed commit is unknown: the target has no record of it")
	}
	head, err := g.repo.Git("rev-parse", "HEAD")
	if err != nil {
		return sha, 0, err
	}
	clean, err := g.repo.IsClean()
	if err != nil {
		return sha, 0, err
	}
	if strings.TrimSpace(head) == sha && clean {
		found, err = g.run(ctx, g.repo.Root)
		return sha, found, err
	}
	dir, err := os.MkdirTemp("", "graft-glint-")
	if err != nil {
		return sha, 0, fmt.Errorf("a work tree for %s: %w", gitx.Short(sha), err)
	}
	defer func() {
		_, rerr := g.repo.Git("worktree", "remove", "--force", dir)
		err = errors.Join(err, rerr, os.RemoveAll(dir))
	}()
	if _, err := g.repo.Git("worktree", "add", "--detach", dir, sha); err != nil {
		return sha, 0, err
	}
	found, err = g.run(ctx, dir)
	return sha, found, err
}

// run is glint check in dir. glint exits non-zero on findings from its own
// fail_on up, so the report decides; a rule that panicked leaves it short.
func (g *glintCheck) run(ctx context.Context, dir string) (int, error) {
	cmd := exec.CommandContext(ctx, "glint", "check", ".", "-s", g.severity, "-o", "json")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var rep glintReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || rep.Summary.Total == nil {
		return 0, fmt.Errorf("glint check gave no report (%v): %s", errors.Join(runErr, err), strings.TrimSpace(stderr.String()))
	}
	if len(rep.Stats.RulesPanicked) > 0 {
		return 0, fmt.Errorf("glint rules panicked: %s", strings.Join(rep.Stats.RulesPanicked, ", "))
	}
	return *rep.Summary.Total, nil
}
