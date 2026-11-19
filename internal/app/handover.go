package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/fsx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// heldItem is an item a deploy left where it was for want of the comment
// require_comment asks for, or for the glint findings; after is the parent it
// followed, "" for an item delivered by its own key.
type heldItem struct {
	key, after string
}

// uninstructed says why the item may not be handed over yet, "" when a
// comment of the token owner written since the item came into the
// transition's from has the text require_comment asks for. A subtask
// following its parent counts the parent's comment too: the one the
// reviewer reads for the whole.
func (m *noteMover) uninstructed(ctx context.Context, issue jira.Issue, t *config.NoteTransition, after string) string {
	keys := []string{issue.Key}
	if after != "" {
		keys = append(keys, after)
	}
	for _, key := range keys {
		since, err := m.client.CameInto(ctx, key, t.From)
		if err != nil {
			return fmt.Sprintf("when the work on %s began is unknown: %v", key, err)
		}
		comments, err := m.client.Comments(ctx, key)
		if err != nil {
			return fmt.Sprintf("the comments of %s are unknown: %v", key, err)
		}
		if slices.ContainsFunc(comments, func(c jira.Comment) bool {
			return c.AuthorID == m.me && !c.Created.Before(since) && t.Instructs(c.Text)
		}) {
			return ""
		}
	}
	where := "it"
	if after != "" {
		where = "it or " + after
	}
	return fmt.Sprintf("no comment with %q by the owner of the Jira token on %s since the work came into %s", t.RequireComment, where, strings.Join(t.From, ", "))
}

// retryHeld moves the items earlier deploys held, but for
// those in done, the keys this deploy has just taken, and returns those it
// could not read: they stay held, as do the items the mover holds again. An
// item that has left the statuses the transitions move from, moved by hand
// say, is held no more.
func (a *App) retryHeld(ctx context.Context, sub *subtaskRun, held []heldItem, done []string) []heldItem {
	return a.retryHeldNamed(ctx, sub, held, done, nil)
}

func (a *App) retryHeldNamed(ctx context.Context, sub *subtaskRun, held []heldItem, done []string, named map[string]bool) []heldItem {
	var unread []heldItem
	for _, h := range held {
		unread = append(unread, a.retryHeldItem(ctx, sub, h, done, named)...)
	}
	return unread
}

func (a *App) retryHeldItem(ctx context.Context, sub *subtaskRun, h heldItem, done []string, named map[string]bool) []heldItem {
	m := sub.mover
	if slices.Contains(done, h.key) || slices.ContainsFunc(m.held, func(x heldItem) bool { return x.key == h.key }) {
		return nil
	}
	issue, err := m.client.Issue(ctx, h.key)
	if err != nil {
		a.warn("%s is still held: %v", h.key, err)
		return []heldItem{h}
	}
	if m.leaveDormant(issue) {
		return nil
	}
	t := m.conf.Moving(issue.Status)
	if t == nil || issue.Done {
		a.printf("%s is held no more: it is %s", h.key, issue.Status)
		return nil
	}
	return a.moveHeldItem(ctx, sub, h, issue, t, named[h.key])
}

func (a *App) moveHeldItem(ctx context.Context, sub *subtaskRun, h heldItem, issue jira.Issue, t *config.NoteTransition, named bool) []heldItem {
	m := sub.mover
	reached := issue.Status
	// A failed assignment is not held: DeployHandover fails on it, as a
	// deploy does.
	moveErr := m.move(ctx, issue, t, h.after)
	if moveErr == nil {
		reached = t.To
	} else if !errors.Is(moveErr, errHeld) && !errors.Is(moveErr, errTypeNotAllowed) {
		if moveErr != nil {
			a.warn("%v", moveErr)
		}
		if named {
			return []heldItem{h}
		}
	}
	if h.after == "" && !errors.Is(moveErr, errTypeNotAllowed) && (!named || moveErr == nil || errors.Is(moveErr, errHeld)) {
		sub.delivered = append(sub.delivered, issue)
		sub.reached[issue.Key] = reached
	}
	return nil
}

// DeployHandover is graft deploy handover <target> [KEY...]: it moves the items the
// target's deploys held for want of their comment, now that it is written,
// or for the glint findings of the deployed commit, now that glint finds
// none or ticket, --glint-ticket, takes them. Items still held make it fail.
func (a *App) DeployHandover(ctx context.Context, target, ticket string, keys ...string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	n := r.target.ReleaseNotes
	if err := validateHandoverNotes(target, n, len(keys) > 0); err != nil {
		return err
	}
	var deployedSHA string
	var deployedErr error
	shaRead := false
	deployedHead := func(ctx context.Context) (string, error) {
		if !shaRead {
			deployedSHA, deployedErr = r.readSHA(ctx, r.env, r.target.DeployedSHA)
			shaRead = true
		}
		return deployedSHA, deployedErr
	}
	if r.glint, err = newGlintCheck(r, ticket, deployedHead); err != nil {
		return err
	}
	held, err := readHeld(r)
	if err != nil {
		return err
	}
	if len(held) == 0 && len(keys) == 0 {
		a.printf("no item of %s is held", target)
		return nil
	}
	if len(keys) > 0 {
		held, err = namedHandoverItems(ctx, r, target, keys, deployedHead, held)
		if err != nil {
			return err
		}
	}
	client, err := jiraClient()
	if err != nil {
		return err
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return err
	}
	mover := &noteMover{app: a, client: client, conf: n, approved: r.target.ApprovedRelease, me: me, target: r.name, glint: r.glint, standing: r.cfg.ReleaseNotes.Jira.SkipKeys}
	sub := &subtaskRun{mover: mover, notes: r.cfg.ReleaseNotes.Jira, me: me, reached: map[string]string{}, run: r, targetHead: deployedHead}
	named := make(map[string]bool, len(keys))
	for _, key := range keys {
		named[key] = true
	}
	mover.namedHandover = named
	unread := a.retryHeldNamed(ctx, sub, held, nil, named)
	if r.target.ReleaseNotes.Subtasks {
		sub.follow(ctx)
	}
	still := append(unread, mover.held...)
	if err := writeHeld(r, still); err != nil {
		return err
	}
	var errs []error
	if len(still) > 0 {
		errs = append(errs, fmt.Errorf("%d of %s are still held (see above)", len(still), target))
	}
	if len(mover.unset.assignee) > 0 {
		errs = append(errs, fmt.Errorf("the assignee is not set on %s (see above)", strings.Join(mover.unset.assignee, ", ")))
	}
	return errors.Join(errs...)
}

func validateHandoverNotes(target string, n *config.TargetNotes, named bool) error {
	if named {
		if n == nil || len(n.Transitions) == 0 {
			return fmt.Errorf("deploy.targets.%s.release_notes.transitions: required to hand over named items", target)
		}
		return nil
	}
	if n == nil || n.Glint == "" && !slices.ContainsFunc(n.Transitions, func(t config.NoteTransition) bool { return t.RequireComment != "" }) {
		return fmt.Errorf("deploy.targets.%s.release_notes: neither glint nor a transition with require_comment holds items", target)
	}
	return nil
}

// namedHandoverItems adds requested keys that have work commits in the
// deployed history, preserving any parent relationship already held.
func namedHandoverItems(ctx context.Context, r *deployRun, target string, keys []string, deployedHead func(context.Context) (string, error), held []heldItem) ([]heldItem, error) {
	deployed, err := deployedHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the deployed commit of %s: %w", target, err)
	}
	keyRe := projectKeyRe(r.cfg)
	var commits []deliveredCommit
	if deployed != "" {
		commits, err = logCommits(r.repo, deployed)
		if err != nil {
			return nil, fmt.Errorf("reading commits of deployed %s: %w", target, err)
		}
	}
	worked := make(map[string]bool)
	for _, c := range commits {
		// isRevert treats a double revert as restored work, matching release notes.
		if c.merge || isRevert(c.subject) {
			continue
		}
		for _, key := range itemKeys(keyRe, r.target.ApprovedRelease, c) {
			worked[key] = true
		}
	}
	for _, key := range keys {
		if key == "" || keyRe.FindString(key) != key {
			return nil, fmt.Errorf("%q is not a work item key for this project's release_notes.jira.project_keys", key)
		}
	}
	parentWorked, err := namedParentWork(ctx, keys, worked)
	if err != nil {
		return nil, fmt.Errorf("reading parents of deployed work items: %w", err)
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !worked[key] && !parentWorked[key] {
			return nil, fmt.Errorf("%s has no work commit in the deployed history of %s", key, target)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if !slices.ContainsFunc(held, func(h heldItem) bool { return h.key == key }) {
			held = append(held, heldItem{key: key})
		}
	}
	return held, nil
}

func namedParentWork(ctx context.Context, keys []string, worked map[string]bool) (map[string]bool, error) {
	var missing []string
	for _, key := range keys {
		if !worked[key] {
			missing = append(missing, key)
		}
	}
	parentWorked := map[string]bool{}
	if len(missing) == 0 || len(worked) == 0 {
		return parentWorked, nil
	}
	client, err := jiraClient()
	if err != nil {
		return nil, err
	}
	children, err := jiraSubtasksByParent(ctx, client, missing)
	if err != nil {
		return nil, err
	}
	for child, parent := range children {
		if worked[child] {
			parentWorked[parent] = true
		}
	}
	return parentWorked, nil
}

// jiraSubtasksByParent returns subtasks of the supplied parent keys with one
// Jira search. The keys were validated as project issue keys.
func jiraSubtasksByParent(ctx context.Context, client *jira.Client, keys []string) (map[string]string, error) {
	keys = uniqueSorted(keys)
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = "\"" + key + "\""
	}
	return client.ReviewSearchSubtasksByParent(ctx, "parent in ("+strings.Join(quoted, ",")+")")
}

// heldPath is the record of the items held on a target, kept with the
// sweep's files.
func heldPath(r *deployRun) (string, error) {
	return sweepPath(r.repo, "held", r.name)
}

// readHeld reads the items held on the target, none without a record.
func readHeld(r *deployRun) ([]heldItem, error) {
	path, err := heldPath(r)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the items held on %s: %w", r.name, err)
	}
	var held []heldItem
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) > 0 {
			h := heldItem{key: f[0]}
			if len(f) > 1 {
				h.after = f[1]
			}
			held = append(held, h)
		}
	}
	return held, nil
}

// writeHeld records the items held on the target, a line each: the key and
// the parent it followed, if any.
func writeHeld(r *deployRun, held []heldItem) error {
	path, err := heldPath(r)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		if err := fsx.Remove(path); err != nil {
			return fmt.Errorf("clearing the items held on %s: %w", r.name, err)
		}
		return nil
	}
	var b strings.Builder
	for _, h := range held {
		b.WriteString(strings.TrimSpace(h.key + " " + h.after))
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("recording the items held on %s: %w", r.name, err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("recording the items held on %s: %w", r.name, err)
	}
	return nil
}
