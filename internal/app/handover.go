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
	m := sub.mover
	var unread []heldItem
	for _, h := range held {
		if slices.Contains(done, h.key) || slices.ContainsFunc(m.held, func(x heldItem) bool { return x.key == h.key }) {
			continue
		}
		issue, err := m.client.Issue(ctx, h.key)
		if err != nil {
			a.warn("%s is still held: %v", h.key, err)
			unread = append(unread, h)
			continue
		}
		t := m.conf.Moving(issue.Status)
		if t == nil || issue.Done {
			a.printf("%s is held no more: it is %s", h.key, issue.Status)
			continue
		}
		reached := issue.Status
		if err := m.move(ctx, issue, t, h.after); err == nil {
			reached = t.To
		} else if !errors.Is(err, errHeld) {
			a.warn("%v", err)
		}
		if h.after == "" {
			sub.delivered = append(sub.delivered, issue)
			sub.reached[issue.Key] = reached
		}
	}
	return unread
}

// DeployHandover is graft deploy handover <target>: it moves the items the
// target's deploys held for want of their comment, now that it is written,
// or for the glint findings of the deployed commit, now that glint finds
// none or ticket, --glint-ticket, takes them. Items still held make it fail.
func (a *App) DeployHandover(ctx context.Context, target, ticket string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	n := r.target.ReleaseNotes
	if n == nil || n.Glint == "" && !slices.ContainsFunc(n.Transitions, func(t config.NoteTransition) bool { return t.RequireComment != "" }) {
		return fmt.Errorf("deploy.targets.%s.release_notes: neither glint nor a transition with require_comment holds items", target)
	}
	if r.glint, err = newGlintCheck(r, ticket, func(ctx context.Context) (string, error) {
		return r.readSHA(ctx, r.env, r.target.DeployedSHA)
	}); err != nil {
		return err
	}
	held, err := readHeld(r)
	if err != nil {
		return err
	}
	if len(held) == 0 {
		a.printf("no item of %s is held", target)
		return nil
	}
	client, err := jiraClient()
	if err != nil {
		return err
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return err
	}
	mover := &noteMover{app: a, client: client, conf: n, me: me, target: r.name, glint: r.glint}
	sub := &subtaskRun{mover: mover, notes: r.cfg.ReleaseNotes.Jira, me: me, reached: map[string]string{}}
	unread := a.retryHeld(ctx, sub, held, nil)
	if r.target.ReleaseNotes.Subtasks {
		sub.follow(ctx)
	}
	still := append(unread, mover.held...)
	if err := writeHeld(r, still); err != nil {
		return err
	}
	if len(still) > 0 {
		return fmt.Errorf("%d of %s are still held (see above)", len(still), target)
	}
	return nil
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
