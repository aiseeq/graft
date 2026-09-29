package flags

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxListed caps the events printed by status; the counts cover all of them.
const maxListed = 30

// Source is a project's journal as seen through its adapter.
type Source interface {
	// Ready reports whether the journal exists at all (false before the
	// migration that creates it has run).
	Ready(ctx context.Context) (bool, error)
	// Open lists every open event, most recently seen first.
	Open(ctx context.Context) ([]Event, error)
	// Get returns one event with its body.
	Get(ctx context.Context, id string) (Event, error)
	// Resolve closes an open event with reason. Closing an event that is not
	// open is an error, never an overwrite of someone else's note.
	Resolve(ctx context.Context, id, reason string) error
}

// Journal is the flags journal of one environment.
type Journal struct {
	Env       string
	Source    Source
	Rules     []Rule
	RulesPath string
	KnownEnvs []string
	Out       io.Writer
}

// Split separates open events into those to review and those an exception
// mutes.
func (j *Journal) Split(events []Event) (review, muted []Event) {
	for _, e := range events {
		if j.muted(e) {
			muted = append(muted, e)
		} else {
			review = append(review, e)
		}
	}
	return review, muted
}

func (j *Journal) muted(e Event) bool {
	for _, r := range j.Rules {
		if r.Matches(j.Env, e) {
			return true
		}
	}
	return false
}

// Status prints whether the flag is raised: any open event no exception mutes.
func (j *Journal) Status(ctx context.Context) error {
	ready, err := j.Source.Ready(ctx)
	if err != nil {
		return err
	}
	if !ready {
		fmt.Fprintf(j.Out, "flag down (%s): the journal does not exist yet\n", j.Env)
		return nil
	}
	events, err := j.Source.Open(ctx)
	if err != nil {
		return err
	}
	review, muted := j.Split(events)
	if len(review) == 0 {
		fmt.Fprintf(j.Out, "flag down (%s): nothing to review, %d muted by exceptions\n", j.Env, len(muted))
		return nil
	}
	fmt.Fprintf(j.Out, "flag RAISED (%s): %d events to review, %d muted by exceptions\n\n", j.Env, len(review), len(muted))
	for i, e := range review {
		if i == maxListed {
			fmt.Fprintf(j.Out, "  ... and %d more\n", len(review)-maxListed)
			break
		}
		fmt.Fprintf(j.Out, "  %s\n    %s\n    %s\n", e.ID, summary(e), e.Subject)
	}
	fmt.Fprintf(j.Out, "\nnext: graft flags show <id> --env %[1]s\n      graft flags ack <id> --reason \"what it was\" --env %[1]s\n      graft flags mute <id> --reason \"why it is noise\" --env %[1]s\n", j.Env)
	return nil
}

func summary(e Event) string {
	parts := []string{e.Class}
	if e.Severity != "" {
		parts = append(parts, e.Severity)
	}
	parts = append(parts, e.Status)
	if e.Times > 0 {
		parts = append(parts, fmt.Sprintf("seen %d times", e.Times))
	}
	switch {
	case e.FirstSeen != "" && e.FirstSeen != e.LastSeen:
		parts = append(parts, e.FirstSeen+" .. "+e.LastSeen)
	default:
		parts = append(parts, e.LastSeen)
	}
	return strings.Join(parts, " · ")
}

// Show prints one event in full.
func (j *Journal) Show(ctx context.Context, id string) error {
	e, err := j.Source.Get(ctx, id)
	if err != nil {
		return err
	}
	line := func(label, value string) {
		if value != "" {
			fmt.Fprintf(j.Out, "%-9s %s\n", label+":", value)
		}
	}
	line("id", e.ID)
	line("class", e.Class)
	line("key", e.Key)
	line("status", e.Status)
	line("severity", e.Severity)
	line("seen", summary(e))
	if j.muted(e) {
		line("muted", "yes, by an exception")
	}
	fmt.Fprintf(j.Out, "\n%s\n", e.Subject)
	if e.Body != "" {
		fmt.Fprintf(j.Out, "\n%s\n", strings.TrimRight(e.Body, "\n"))
	}
	if e.Note != "" {
		by := ""
		if e.NotedBy != "" {
			by = " (" + e.NotedBy + ", " + e.NotedAt + ")"
		}
		fmt.Fprintf(j.Out, "\nnote%s: %s\n", by, e.Note)
	}
	return nil
}

// Ack closes an open event with a reason.
func (j *Journal) Ack(ctx context.Context, id, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("--reason is required: say what the event was")
	}
	if err := j.Source.Resolve(ctx, id, reason); err != nil {
		return err
	}
	fmt.Fprintf(j.Out, "closed %s (%s): %s\n", id, j.Env, reason)
	return nil
}

// MuteOptions are the arguments of graft flags mute.
type MuteOptions struct {
	ID     string
	Reason string
	// Match narrows the rule to part of the subject; empty means the whole
	// subject.
	Match string
	// AllEnvs writes the rule for every environment instead of this one.
	AllEnvs bool
}

// Mute records an exception for the event's class and subject, then closes
// the event. The rule is written first: if closing fails, the rule still stops
// the next occurrence from raising the flag, and the error says so.
func (j *Journal) Mute(ctx context.Context, o MuteOptions) error {
	if strings.TrimSpace(o.Reason) == "" {
		return errors.New("--reason is required: say why this is noise")
	}
	e, err := j.Source.Get(ctx, o.ID)
	if err != nil {
		return err
	}
	match := o.Match
	if match == "" {
		match = e.Subject
	}
	if match != WholeClass && !strings.Contains(e.Subject, match) {
		return fmt.Errorf("--match %q is not part of the subject %q", match, e.Subject)
	}
	rule := Rule{Class: e.Class, Substring: strings.TrimSpace(match), Reason: strings.TrimSpace(o.Reason)}
	if !o.AllEnvs {
		rule.Envs = []string{j.Env}
	}
	added, err := AppendRule(j.RulesPath, rule, j.KnownEnvs)
	if err != nil {
		return err
	}
	if added {
		fmt.Fprintf(j.Out, "added to %s: %s\n", j.RulesPath, rule)
	} else {
		fmt.Fprintf(j.Out, "%s already has this rule\n", j.RulesPath)
	}
	if err := j.Source.Resolve(ctx, o.ID, o.Reason); err != nil {
		if errors.Is(err, ErrNotOpen) {
			fmt.Fprintf(j.Out, "%s was already closed; the rule still applies to its next occurrence\n", o.ID)
			return nil
		}
		return fmt.Errorf("rule written, but closing the event failed: %w", err)
	}
	fmt.Fprintf(j.Out, "closed %s (%s); commit %s\n", o.ID, j.Env, j.RulesPath)
	return nil
}
