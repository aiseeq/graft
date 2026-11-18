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

// commitTicketHints gives advisory Jira hints for the keys named by the
// prepared commit message. It never changes whether the commit can proceed.
func (a *App) commitTicketHints(ctx context.Context, cfg *config.Config, msg string) {
	if cfg.ReleaseNotes == nil || cfg.ReleaseNotes.Jira == nil || cfg.Ticket == nil || cfg.Ticket.Regexp == nil {
		return
	}
	keys := commitHintKeys(cfg, msg)
	if len(keys) == 0 {
		return
	}
	client, err := jiraClient()
	if err != nil {
		writeCommitHintFailures(a, keys, commitHintFailure(err, true))
		return
	}

	readCtx, cancel := context.WithTimeout(ctx, commitTicketHintTimeout)
	defer cancel()
	issues, me := readCommitHintBatch(readCtx, client, keys)
	for _, key := range keys {
		read, ok := issues[key]
		if !ok {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: timeout\n", key)
			continue
		}
		if read.err != nil {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: %s\n", key, commitHintFailure(read.err, false))
			continue
		}
		if me.err != nil {
			fmt.Fprintf(a.Stderr, "graft: %s not checked in Jira: %s\n", key, commitHintFailure(me.err, false))
			a.commitTicketIssueHints(cfg, key, read.issue, "", false)
			continue
		}
		a.commitTicketIssueHints(cfg, key, read.issue, me.account, true)
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
