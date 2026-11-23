package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jira"
	"github.com/aiseeq/graft/v2/internal/message"
)

const commitTicketHintTimeout = 3 * time.Second
const commitTicketHintMaxKeys = 5

type commitHintRead struct {
	key   string
	issue jira.Issue
	err   error
}

type commitHintMyself struct {
	account string
	err     error
}

type commitTicketHintBatch struct {
	keys    []string
	issues  map[string]commitHintRead
	myself  commitHintMyself
	failure string
}

func (a *App) readCommitTicketHints(ctx context.Context, cfg *config.Config, msg string) commitTicketHintBatch {
	var batch commitTicketHintBatch
	if cfg.ReleaseNotes == nil || cfg.ReleaseNotes.Jira == nil || cfg.Ticket == nil || cfg.Ticket.Regexp == nil {
		return batch
	}
	keys := commitHintKeys(cfg, msg)
	if len(keys) == 0 {
		return batch
	}
	batch.keys = keys
	client, err := jiraClient()
	if err != nil {
		batch.failure = commitHintFailure(err, true)
		return batch
	}

	readCtx, cancel := context.WithTimeout(ctx, commitTicketHintTimeout)
	defer cancel()
	batch.issues, batch.myself = readCommitHintBatch(readCtx, client, keys)
	return batch
}

func (a *App) writeCommitTicketHints(cfg *config.Config, batch commitTicketHintBatch) {
	if batch.failure != "" {
		writeCommitHintFailures(a, batch.keys, batch.failure)
		return
	}
	for _, key := range batch.keys {
		read, ok := batch.issues[key]
		if !ok {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: timeout\n", key)
			continue
		}
		if read.err != nil {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: %s\n", key, commitHintFailure(read.err, false))
			continue
		}
		if batch.myself.err != nil {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: %s\n", key, commitHintFailure(batch.myself.err, false))
			a.commitTicketIssueHints(cfg, key, read.issue, "", false)
			continue
		}
		a.commitTicketIssueHints(cfg, key, read.issue, batch.myself.account, true)
	}
}

func readCommitHintBatch(ctx context.Context, client *jira.Client, keys []string) (map[string]commitHintRead, commitHintMyself) {
	reads := make(chan commitHintRead, len(keys))
	for _, key := range keys {
		go func(key string) {
			issue, err := client.Issue(ctx, key)
			reads <- commitHintRead{key: key, issue: issue, err: err}
		}(key)
	}
	myself := make(chan commitHintMyself, 1)
	go func() {
		account, err := client.Myself(ctx)
		myself <- commitHintMyself{account: account, err: err}
	}()

	issues := make(map[string]commitHintRead, len(keys))
	var me commitHintMyself
	gotMyself := false
	for len(issues) < len(keys) || !gotMyself {
		select {
		case read := <-reads:
			issues[read.key] = read
		case me = <-myself:
			gotMyself = true
		case <-ctx.Done():
			goto collectReady
		}
	}

collectReady:
	for {
		select {
		case read := <-reads:
			issues[read.key] = read
		default:
			goto collectMyself
		}
	}

collectMyself:
	if !gotMyself {
		select {
		case me = <-myself:
		default:
			me.err = ctx.Err()
		}
	}
	return issues, me
}

func commitHintKeys(cfg *config.Config, msg string) []string {
	keys := make([]string, 0, commitTicketHintMaxKeys)
	for _, candidate := range message.WordKeys(msg, cfg.Ticket.Regexp) {
		project, _, ok := strings.Cut(candidate, "-")
		if !ok || !slices.Contains(cfg.ReleaseNotes.Jira.ProjectKeys, project) || slices.Contains(keys, candidate) {
			continue
		}
		keys = append(keys, candidate)
		if len(keys) == commitTicketHintMaxKeys {
			break
		}
	}
	return keys
}

func (a *App) commitTicketIssueHints(cfg *config.Config, key string, issue jira.Issue, me string, assigneeKnown bool) {
	if assigneeKnown {
		if issue.AssigneeID == "" {
			fmt.Fprintf(a.Stderr, "graft: %s has no assignee; the commit lands in its Development. Your own task or a subtask takes another key\n", key)
		} else if issue.AssigneeID != me {
			name := issue.AssigneeName
			if name == "" {
				name = issue.AssigneeID
			}
			fmt.Fprintf(a.Stderr, "graft: %s is %s 's task; the commit lands in its Development. Your own task or a subtask takes another key\n", key, oneLineHint(name))
		}
	}
	if issue.Subtask || cfg.Deploy == nil {
		return
	}
	// A deploy leaves a release_notes.jira.skip_keys task where it is
	// (noteItems.post), so the commit names no move for it.
	if cfg.ReleaseNotes != nil && cfg.ReleaseNotes.Jira != nil && slices.Contains(cfg.ReleaseNotes.Jira.SkipKeys, key) {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Deploy.Targets)) {
		target := cfg.Deploy.Targets[name]
		if target.ReleaseNotes == nil {
			continue
		}
		to, _ := target.ReleaseNotes.DeliveredStatus(issue.Status, issue.Type)
		if to != issue.Status {
			fmt.Fprintf(a.Stderr, "graft: deploying to %s moves %s to %s; an intermediate commit goes under a subtask key\n", name, key, to)
		}
	}
}

func (a *App) writeNonReleaseParentHints(cfg *config.Config, batch commitTicketHintBatch, paths []string, loadBumped func() ([]string, error)) {
	if cfg.Deploy == nil || len(paths) == 0 || batch.failure != "" {
		return
	}
	var bumped []string
	loaded := false
	for _, key := range batch.keys {
		read, ok := batch.issues[key]
		if !ok {
			continue
		}
		if read.err != nil {
			continue
		}
		if !read.issue.Subtask || read.issue.Parent == "" {
			continue
		}
		var targets []string
		for _, name := range slices.Sorted(maps.Keys(cfg.Deploy.Targets)) {
			approved := cfg.Deploy.Targets[name].ApprovedRelease
			if approved == nil || !slices.Contains(approved.NonReleaseParents, read.issue.Parent) {
				continue
			}
			if !loaded {
				loaded = true
				var err error
				bumped, err = loadBumped()
				if err != nil {
					a.warn("cannot classify staged version files for release hint: %v", err)
				}
			}
			files, _ := classifyShipChanges(paths, bumped, approved)
			if len(files) > 0 {
				targets = append(targets, name)
			}
		}
		if len(targets) == 0 {
			continue
		}
		fmt.Fprintf(a.Stderr, "graft: %s is a subtask of %s, which does not go to release (approved_release.non_release_parents of %s); code for the release goes under a subtask of the release task\n", key, read.issue.Parent, strings.Join(targets, ", "))
	}
}

func commitHintFailure(err error, credentials bool) string {
	if credentials {
		return "no token"
	}
	var apiErr *jira.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("HTTP %d", apiErr.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "network"
}

func writeCommitHintFailures(a *App, keys []string, reason string) {
	for _, key := range keys {
		fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: %s\n", key, reason)
	}
}

func oneLineHint(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}
