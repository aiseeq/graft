package flags

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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
	for i := range events {
		if j.muted(events[i]) {
			muted = append(muted, events[i])
		} else {
			review = append(review, events[i])
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

// Count is what an environment's journal holds against the flag.
type Count struct {
	// Ready is false before the journal exists.
	Ready         bool
	Review, Muted []Event
}

// Count reads the open events and splits them by the exceptions.
func (j *Journal) Count(ctx context.Context) (Count, error) {
	ready, err := j.Source.Ready(ctx)
	if err != nil || !ready {
		return Count{}, err
	}
	classes := j.bodyClasses()
	var events []Event
	if bo, ok := j.Source.(BodiesOpener); ok && len(classes) > 0 {
		events, err = bo.OpenWithBodies(ctx, classes)
	} else {
		events, err = j.Source.Open(ctx)
	}
	if err != nil {
		return Count{}, err
	}
	if err := j.fillBodies(ctx, events, classes); err != nil {
		return Count{}, err
	}
	review, muted := j.Split(events)
	return Count{Ready: true, Review: review, Muted: muted}, nil
}

// BodiesOpener is a source that can list open events with the bodies of some
// classes in one read, where reading them one at a time costs a call each.
type BodiesOpener interface {
	OpenWithBodies(ctx context.Context, classes []string) ([]Event, error)
}

// bodyClasses are the classes a body rule of this environment names.
func (j *Journal) bodyClasses() []string {
	var classes []string
	for _, r := range j.Rules {
		if r.In == InBody && (r.Envs == nil || slices.Contains(r.Envs, j.Env)) && !slices.Contains(classes, r.Class) {
			classes = append(classes, r.Class)
		}
	}
	return classes
}

// fillBodies reads the body of each event of the classes given where the
// listing left it out: a source lists events without their bodies, and a body
// rule would otherwise never match.
func (j *Journal) fillBodies(ctx context.Context, events []Event, classes []string) error {
	for i := range events {
		e := &events[i]
		if !slices.Contains(classes, e.Class) || e.Body != "" {
			continue
		}
		full, err := j.Source.Get(ctx, e.ID)
		if err != nil {
			return fmt.Errorf("reading the body of %s for the exceptions: %w", e.ID, err)
		}
		e.Body = full.Body
	}
	return nil
}

// Raised tells whether any open event is left after the exceptions.
func (c Count) Raised() bool { return len(c.Review) > 0 }

// Headline is the one line that says whether the flag of env is raised.
func (c Count) Headline(env string) string {
	switch {
	case !c.Ready:
		return fmt.Sprintf("flag down (%s): the journal does not exist yet", env)
	case !c.Raised():
		return fmt.Sprintf("flag down (%s): nothing to review, %d muted by exceptions", env, len(c.Muted))
	default:
		return fmt.Sprintf("flag RAISED (%s): %d events to review, %d muted by exceptions", env, len(c.Review), len(c.Muted))
	}
}

// Status prints whether the flag is raised: any open event no exception mutes.
func (j *Journal) Status(ctx context.Context) error {
	c, err := j.Count(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(j.Out, c.Headline(j.Env))
	if !c.Raised() {
		return nil
	}
	fmt.Fprintln(j.Out)
	review := c.Review
	for i := range review {
		if i == maxListed {
			fmt.Fprintf(j.Out, "  ... and %d more\n", len(review)-maxListed)
			break
		}
		e := &review[i]
		fmt.Fprintf(j.Out, "  %s\n    %s\n    %s\n", e.ID, summary(*e), e.Subject)
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
	// Match narrows the rule to part of the subject, or of the field In
	// names; empty means the whole subject or key.
	Match string
	// In is the field Match is part of: InSubject (empty), InBody or InKey.
	In string
	// AllEnvs writes the rule for every environment instead of this one.
	AllEnvs bool
}

// Mute records an exception for the event's class and subject, or the part
// of its body or key given, then closes the event. The rule is written first:
// if closing fails, the rule still stops the next occurrence from raising the
// flag, and the error says so.
func (j *Journal) Mute(ctx context.Context, o MuteOptions) error {
	if strings.TrimSpace(o.Reason) == "" {
		return errors.New("--reason is required: say why this is noise")
	}
	e, err := j.Source.Get(ctx, o.ID)
	if err != nil {
		return err
	}
	rule := Rule{Class: e.Class, In: o.In, Reason: strings.TrimSpace(o.Reason)}
	in, text := "subject", e.Subject
	switch o.In {
	case InSubject:
	case InBody:
		if o.Match == "" {
			return errors.New("--in body needs --match: the part of the body that tells this event from others of its class")
		}
		in, text = InBody, e.Body
	case InKey:
		in, text = InKey, e.Key
	default:
		return fmt.Errorf("--in %q: want subject, body or key", o.In)
	}
	match := o.Match
	if match == "" {
		match = text
	}
	if match == "" {
		return fmt.Errorf("event %s has no %s to match", o.ID, in)
	}
	if o.In != InSubject && match == WholeClass {
		return fmt.Errorf("--match * mutes the whole class: drop --in %s", in)
	}
	if match != WholeClass && !strings.Contains(text, match) {
		return fmt.Errorf("--match %q is not part of the %s %q", match, in, truncate(text, 300))
	}
	rule.Substring = strings.TrimSpace(match)
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
