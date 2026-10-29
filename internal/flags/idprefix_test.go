package flags

// The id resolution of show, ack and mute: an open event's id or a prefix
// only one open event has reaches the source as the full id; anything else
// fails before the source is asked.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recSource records the ids that reach the single-event calls.
type recSource struct {
	memSource
	got      []string
	resolved []string
}

func (s *recSource) Get(ctx context.Context, id string) (Event, error) {
	s.got = append(s.got, id)
	return s.memSource.Get(ctx, id)
}

func (s *recSource) Resolve(ctx context.Context, id, reason string) error {
	s.resolved = append(s.resolved, id)
	return s.memSource.Resolve(ctx, id, reason)
}

func prefixJournal(t *testing.T, ids ...string) (*Journal, *recSource, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exceptions.conf")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	src := &recSource{}
	for i, id := range ids {
		e := negativeTotal(id, "u"+string(rune('a'+i)))
		e.Subject += " " + id
		src.events = append(src.events, e)
	}
	var out bytes.Buffer
	return &Journal{Env: "test", Source: src, RulesPath: path, KnownEnvs: knownEnvs, Out: &out}, src, &out
}

const (
	idA = "8e7af728-1b2c-4d5e-8f90-0123456789ab"
	idB = "1f00d000-aaaa-4bbb-8ccc-dddddddddddd"
	idC = "8e7a0000-ffff-4eee-8ddd-cccccccccccc"
)

func TestIDPrefixReachesSourceAsFullID(t *testing.T) {
	ctx := context.Background()
	j, src, out := prefixJournal(t, idA, idB)
	if err := j.Show(ctx, "8e7af728"); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), idA) {
		t.Errorf("show output lacks the full id:\n%s", out)
	}
	if err := j.Ack(ctx, "8e7af7", "seen"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if !strings.Contains(out.String(), "closed "+idA) {
		t.Errorf("ack output must name the full id:\n%s", out)
	}
	if err := j.Mute(ctx, MuteOptions{ID: "1f00", Reason: "noise", In: InKey}); err != nil {
		t.Fatalf("mute: %v", err)
	}
	for _, id := range append(append([]string{}, src.got...), src.resolved...) {
		if id != idA && id != idB {
			t.Errorf("the source got %q, want a full id", id)
		}
	}
	if src.events[0].Status != "resolved" || src.events[1].Status != "resolved" {
		t.Errorf("statuses %q %q, want both resolved", src.events[0].Status, src.events[1].Status)
	}
}

func TestIDPrefixAmbiguousOrMissingNeverReachesSource(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, id string }{{"ambiguous", "8e7a"}, {"missing", "dead"}} {
		j, src, _ := prefixJournal(t, idA, idB, idC)
		errs := []error{
			j.Show(ctx, tc.id),
			j.Ack(ctx, tc.id, "r"),
			j.Mute(ctx, MuteOptions{ID: tc.id, Reason: "r", In: InKey}),
		}
		for i, err := range errs {
			if err == nil {
				t.Errorf("%s: call %d: want an error", tc.name, i)
				continue
			}
			if tc.name == "ambiguous" && (!strings.Contains(err.Error(), idA) || !strings.Contains(err.Error(), idC)) {
				t.Errorf("ambiguous: call %d: the error must list both candidates: %v", i, err)
			}
		}
		if len(src.got) != 0 || len(src.resolved) != 0 {
			t.Errorf("%s: the source was asked for %v / resolved %v", tc.name, src.got, src.resolved)
		}
		data, _ := os.ReadFile(j.RulesPath)
		if len(data) != 0 {
			t.Errorf("%s: a refused mute wrote a rule: %s", tc.name, data)
		}
	}
}

func TestIDExactWinsOverLongerOne(t *testing.T) {
	ctx := context.Background()
	j, src, _ := prefixJournal(t, "e1", "e10")
	if err := j.Ack(ctx, "e1", "seen"); err != nil {
		t.Fatalf("ack e1: %v", err)
	}
	if len(src.resolved) != 1 || src.resolved[0] != "e1" {
		t.Errorf("resolved %v, want [e1]", src.resolved)
	}
}

// TestIDFullUUIDReachesClosedEvent: a full UUID no open event has goes to the
// source as before, so a closed event can still be shown, and a mute of it
// writes the rule for its next occurrence.
func TestIDFullUUIDReachesClosedEvent(t *testing.T) {
	ctx := context.Background()
	j, src, out := prefixJournal(t, idA, idB)
	src.events[0].Status = "resolved"
	if err := j.Show(ctx, idA); err != nil {
		t.Fatalf("show a closed event by its full id: %v", err)
	}
	if !strings.Contains(out.String(), idA) {
		t.Errorf("show output:\n%s", out)
	}
	if err := j.Mute(ctx, MuteOptions{ID: idA, Reason: "noise", In: InKey}); err != nil {
		t.Fatalf("mute a closed event by its full id: %v", err)
	}
	if !strings.Contains(out.String(), "was already closed") {
		t.Errorf("mute output:\n%s", out)
	}
	if err := j.Ack(ctx, idA, "seen"); err == nil {
		t.Error("ack of a closed event: want the source's error")
	}
	if err := j.Show(ctx, idA[:8]); err == nil {
		t.Error("a prefix of a closed event only: want an error")
	}
}
