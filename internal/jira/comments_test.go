package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestCommentsReadsEveryPageAsText puts the second comment on a page of its
// own and flattens a body of a heading split by marks, a list and a mention.
func TestCommentsReadsEveryPageAsText(t *testing.T) {
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	para := func(nodes ...any) map[string]any { return map[string]any{"type": "paragraph", "content": nodes} }
	body := map[string]any{"type": "doc", "version": 1, "content": []any{
		para(map[string]any{"type": "mention", "attrs": map[string]string{"id": "qa"}}, text(" the form now saves.")),
		para(text("How to "), map[string]any{"type": "text", "text": "check", "marks": []any{map[string]string{"type": "strong"}}}, text(" on test:")),
		map[string]any{"type": "bulletList", "content": []any{
			map[string]any{"type": "listItem", "content": []any{para(text("open the form"))}},
			map[string]any{"type": "listItem", "content": []any{para(text("press Save"), map[string]any{"type": "hardBreak"}, text("see it saved"))}},
		}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startAt"))
		if err != nil || r.URL.Path != "/rest/api/3/issue/PROJ-1/comment" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		comment := map[string]any{"author": map[string]string{"accountId": "me"}, "created": "2026-01-02T10:00:00.000+0200", "body": body}
		if start == 1 {
			comment = map[string]any{"author": map[string]string{"accountId": "qa"}, "created": "2026-01-03T09:30:00.500+0000", "body": map[string]any{"type": "doc", "content": []any{para(text("how?"))}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 2, "comments": []any{comment}})
	}))
	defer srv.Close()
	got, err := New(Credentials{BaseURL: srv.URL}).Comments(context.Background(), "PROJ-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("comments = %+v %v", got, err)
	}
	want := "the form now saves.\nHow to check on test:\nopen the form\npress Save\nsee it saved"
	if got[0].AuthorID != "me" || got[0].Text != want || !got[0].Created.Equal(time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("first = %+v, want text %q", got[0], want)
	}
	if got[1].AuthorID != "qa" || got[1].Text != "how?" {
		t.Errorf("second = %+v", got[1])
	}
}

// TestCameIntoTakesTheLastEntryFromOutside: a move between two of the
// statuses is no new start, a return from review is.
func TestCameIntoTakesTheLastEntryFromOutside(t *testing.T) {
	entry := func(created, from, to string) map[string]any {
		return map[string]any{"created": created, "items": []any{
			map[string]any{"fieldId": "assignee", "fromString": "a", "toString": "b"},
			map[string]any{"fieldId": "status", "fromString": from, "toString": to},
		}}
	}
	pages := [][]any{
		{entry("2026-01-01T10:00:00.000+0000", "To Do", "In Progress"), entry("2026-01-02T10:00:00.000+0000", "In Progress", "Testing")},
		{entry("2026-01-03T10:00:00.000+0000", "Testing", "In Progress"), entry("2026-01-04T10:00:00.000+0000", "In Progress", "Review")},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startAt"))
		if err != nil || start%2 != 0 || start/2 >= len(pages) {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"isLast": start/2 == len(pages)-1, "values": pages[start/2]})
	}))
	defer srv.Close()
	c := New(Credentials{BaseURL: srv.URL})
	got, err := c.CameInto(context.Background(), "PROJ-1", []string{"In Progress", "Review"})
	if want := time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC); err != nil || !got.Equal(want) {
		t.Errorf("came into = %v %v, want %v", got, err, want)
	}
	got, err = c.CameInto(context.Background(), "PROJ-1", []string{"Blocked"})
	if err != nil || !got.IsZero() {
		t.Errorf("never blocked: %v %v", got, err)
	}
}
