package flags

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memSource lists events without their bodies, as the SQL source does, and
// counts the reads of single events.
type memSource struct {
	events []Event
	gets   int
}

func (s *memSource) Ready(context.Context) (bool, error) { return true, nil }

func (s *memSource) Open(context.Context) ([]Event, error) {
	var open []Event
	for _, e := range s.events {
		if e.Status == "open" {
			e.Body = ""
			open = append(open, e)
		}
	}
	return open, nil
}

func (s *memSource) Get(_ context.Context, id string) (Event, error) {
	s.gets++
	for _, e := range s.events {
		if e.ID == id {
			return e, nil
		}
	}
	return Event{}, ErrNotOpen
}

func (s *memSource) Resolve(_ context.Context, id, reason string) error {
	for i := range s.events {
		if s.events[i].ID == id && s.events[i].Status == "open" {
			s.events[i].Status, s.events[i].Note = "resolved", reason
			return nil
		}
	}
	return ErrNotOpen
}

func negativeTotal(id, user string) Event {
	return Event{ID: id, Class: "log_error", Subject: "balance check: negative total",
		Body: "total=-0.05 userID=" + user, Key: "balance:" + user, Status: "open"}
}

func TestBodyRuleMutesOneObject(t *testing.T) {
	src := &memSource{events: []Event{negativeTotal("e1", "f00d"), negativeTotal("e2", "beef"),
		{ID: "e3", Class: "http_failure", Subject: "timeout", Status: "open"}}}
	rules, err := ParseRules([]byte("test|log_error|body:userID=f00d|one account's data\n"), knownEnvs)
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{Env: "test", Source: src, Rules: rules, Out: &bytes.Buffer{}}
	c, err := j.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Muted) != 1 || c.Muted[0].ID != "e1" || len(c.Review) != 2 {
		t.Errorf("muted %+v, review %+v", c.Muted, c.Review)
	}
	if src.gets != 2 {
		t.Errorf("%d bodies read, want the 2 of the class with a body rule", src.gets)
	}

	src.gets = 0
	j.Env = "prod"
	if c, err := j.Count(context.Background()); err != nil || len(c.Muted) != 0 || src.gets != 0 {
		t.Errorf("a rule of another env reads no bodies and mutes nothing: %+v, %d reads, %v", c.Muted, src.gets, err)
	}
}

func TestMuteIn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "exceptions.conf")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	src := &memSource{events: []Event{negativeTotal("e1", "f00d"), negativeTotal("e2", "beef"), negativeTotal("e3", "cafe")}}
	var out bytes.Buffer
	j := &Journal{Env: "test", Source: src, RulesPath: path, KnownEnvs: knownEnvs, Out: &out}
	if err := j.Mute(ctx, MuteOptions{ID: "e1", Reason: "test data", In: InBody, Match: "userID=f00d"}); err != nil {
		t.Fatal(err)
	}
	if err := j.Mute(ctx, MuteOptions{ID: "e2", Reason: "test data", In: InKey}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "test|log_error|body:userID=f00d|test data\ntest|log_error|key:balance:beef|test data\n" {
		t.Errorf("file:\n%s", data)
	}
	for name, o := range map[string]MuteOptions{
		"body without match": {ID: "e3", Reason: "r", In: InBody},
		"not in the body":    {ID: "e3", Reason: "r", In: InBody, Match: "userID=f00d"},
		"whole class in key": {ID: "e3", Reason: "r", In: InKey, Match: "*"},
		"unknown field":      {ID: "e3", Reason: "r", In: "note", Match: "x"},
	} {
		if err := j.Mute(ctx, o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if src.events[2].Status != "open" {
		t.Error("a refused mute closed the event")
	}
	if !strings.Contains(out.String(), "closed e1") {
		t.Errorf("output:\n%s", out.String())
	}
}
