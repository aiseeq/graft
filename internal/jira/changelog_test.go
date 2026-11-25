package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestWasAssignedToReadsEveryPage puts the hand-over on the second page.
func TestWasAssignedToReadsEveryPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startAt"))
		if err != nil {
			http.Error(w, "no startAt", http.StatusBadRequest)
			return
		}
		item := map[string]any{"fieldId": "status", "from": "1", "to": "2"}
		if start == 1 {
			item = map[string]any{"fieldId": "assignee", "from": "me", "to": "reviewer"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"isLast":  start == 1,
			"startAt": start,
			"total":   2,
			"values":  []any{map[string]any{"items": []any{item}}},
		})
	}))
	defer srv.Close()
	c := New(Credentials{BaseURL: srv.URL})
	for account, want := range map[string]bool{"me": true, "reviewer": true, "colleague": false} {
		got, err := c.WasAssignedTo(context.Background(), "PROJ-1", account)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", account, got, err, want)
		}
	}
}

func TestStatusChangesRejectsLastPageWithoutCompleteness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"startAt": 0,
			"isLast":  true,
			"values": []any{map[string]any{
				"created": "2026-11-25T10:00:00.000+0000",
				"author":  map[string]string{"accountId": "reviewer", "displayName": "Synthetic reviewer"},
				"items":   []any{map[string]string{"fieldId": "status", "fromString": "Testing", "toString": "Ready for release"}},
			}},
		})
	}))
	defer srv.Close()

	changes, err := New(Credentials{BaseURL: srv.URL}).StatusChanges(context.Background(), "PROJ-123")
	if err == nil || changes != nil {
		t.Fatalf("StatusChanges = %+v, %v; want incomplete changelog refusal", changes, err)
	}
}

func TestStatusChangesPreservesTransitionsWithinOneHistoryEntry(t *testing.T) {
	want := []struct{ from, to string }{
		{"In Progress", "Testing"},
		{"Testing", "Ready for release"},
		{"Ready for release", "In Progress"},
		{"In Progress", "Ready for release"},
		{"Ready for release", "Done"},
		{"Done", "In Progress"},
		{"In Progress", "Done"},
		{"Done", "Testing"},
		{"Testing", "Done"},
		{"Done", "Ready for release"},
		{"Ready for release", "In Progress"},
	}
	middleItems := make([]any, 0, len(want)-2)
	for _, move := range want[1 : len(want)-1] {
		middleItems = append(middleItems, map[string]string{"fieldId": "status", "fromString": move.from, "toString": move.to})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"startAt": 0, "total": 3, "isLast": true, "values": []any{
			map[string]any{"created": "2026-11-25T10:00:00.000+0000", "items": []any{map[string]string{"fieldId": "status", "fromString": want[0].from, "toString": want[0].to}}},
			map[string]any{"created": "2026-11-25T10:01:00.000+0000", "items": middleItems},
			map[string]any{"created": "2026-11-25T10:02:00.000+0000", "items": []any{map[string]string{"fieldId": "status", "fromString": want[len(want)-1].from, "toString": want[len(want)-1].to}}},
		}})
	}))
	defer srv.Close()

	changes, err := New(Credentials{BaseURL: srv.URL}).StatusChanges(context.Background(), "PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != len(want) {
		t.Fatalf("got %d transitions, want %d", len(changes), len(want))
	}
	for i, move := range want {
		if changes[i].From != move.from || changes[i].To != move.to {
			t.Errorf("transition %d = %s -> %s, want %s -> %s", i, changes[i].From, changes[i].To, move.from, move.to)
		}
	}
}

func TestFixVersionHistoryAfterUsesLatestAddition(t *testing.T) {
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	entry := func(created, from, to string) map[string]any {
		return map[string]any{"created": created, "items": []any{
			map[string]any{"fieldId": "fixVersions", "from": from, "to": to},
		}}
	}
	cases := []struct {
		name    string
		entries []any
		want    bool
	}{
		{name: "removed then re-added after freeze", entries: []any{
			entry("2026-01-01T12:00:00.000+0000", "", "10010"),
			entry("2026-01-03T12:00:00.000+0000", "10010", ""),
			entry("2026-01-04T12:00:00.000+0000", "", "10010"),
		}, want: true},
		{name: "added before freeze", entries: []any{
			entry("2026-01-01T12:00:00.000+0000", "", "10010"),
		}, want: false},
		{name: "another version changed", entries: []any{
			entry("2026-01-04T12:00:00.000+0000", "", "10011"),
		}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/rest/api/3/issue/PROJ-1/changelog" || r.URL.Query().Get("startAt") != "0" {
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"startAt": 0, "total": len(tc.entries), "isLast": true, "values": tc.entries})
			}))
			defer srv.Close()
			got, _, err := New(Credentials{BaseURL: srv.URL}).FixVersionHistoryAfter(context.Background(), "PROJ-1", "10010", at)
			if err != nil || got != tc.want {
				t.Errorf("FixVersionHistoryAfter = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestLastFixVersionRemovalFindsLatestRemoval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"startAt": 0, "total": 4, "isLast": true, "values": []any{
			map[string]any{"created": "2026-11-16T15:55:00.000+0000", "author": map[string]string{"displayName": "Jane Doe"}, "items": []any{map[string]string{"fieldId": "fixVersions", "from": "10010", "fromString": "Product 1", "to": ""}}},
			map[string]any{"created": "2026-11-16T15:40:00.000+0000", "author": map[string]string{"displayName": "Someone Else"}, "items": []any{map[string]string{"fieldId": "fixVersions", "from": "10010", "fromString": "Product 1", "to": ""}}},
			map[string]any{"created": "2026-11-16T16:10:00.000+0000", "author": map[string]string{"displayName": "Other Version"}, "items": []any{map[string]string{"fieldId": "fixVersions", "from": "10020", "fromString": "Other 2", "to": ""}}},
			map[string]any{"created": "2026-11-16T16:20:00.000+0000", "author": map[string]string{"displayName": "Adder"}, "items": []any{map[string]string{"fieldId": "fixVersions", "from": "", "to": "10010", "toString": "Product 1"}}},
		}})
	}))
	defer srv.Close()
	client := New(Credentials{BaseURL: srv.URL})
	got, found, err := client.LastFixVersionRemoval(context.Background(), "PROJ-1", func(id, _ string) bool { return id == "10010" })
	if err != nil || !found || got != (FixVersionRemoval{Version: "Product 1", Who: "Jane Doe", When: "2026-11-16 15:55"}) {
		t.Fatalf("removal = %+v %v %v", got, found, err)
	}
	got, found, err = client.LastFixVersionRemoval(context.Background(), "PROJ-1", func(_, name string) bool { return strings.HasPrefix(name, "Product ") })
	if err != nil || !found || got.Who != "Jane Doe" {
		t.Fatalf("removal by name = %+v %v %v", got, found, err)
	}
	if _, found, err = client.LastFixVersionRemoval(context.Background(), "PROJ-1", func(id, _ string) bool { return id == "99" }); err != nil || found {
		t.Fatalf("removal of a version never taken out: found=%v err=%v", found, err)
	}
}

func TestVersionApprovers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/version/10010" || r.URL.Query().Get("expand") != "approvers" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "10010", "approvers": []any{
			map[string]string{"accountId": "a", "status": "APPROVED"},
			map[string]string{"accountId": "b", "status": "PENDING"},
		}})
	}))
	defer srv.Close()
	c := New(Credentials{BaseURL: srv.URL})
	got, err := c.VersionApprovers(context.Background(), "10010")
	if err != nil || len(got) != 2 || got[0] != (Approver{AccountID: "a", Status: "APPROVED"}) || got[1].Status != "PENDING" {
		t.Errorf("approvers = %+v %v", got, err)
	}
	if _, err := c.VersionApprovers(context.Background(), "10011"); err == nil {
		t.Error("an unknown version read without an error")
	}
}

func TestUserName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch id := r.URL.Query().Get("accountId"); {
		case r.URL.Path != "/rest/api/3/user":
			http.Error(w, "unexpected", http.StatusNotFound)
		case id == "a:1":
			_ = json.NewEncoder(w).Encode(map[string]string{"accountId": id, "displayName": "Ann Example"})
		case id == "blank":
			_ = json.NewEncoder(w).Encode(map[string]string{"accountId": id, "displayName": "   "})
		case id == "other":
			_ = json.NewEncoder(w).Encode(map[string]string{"accountId": "a:1", "displayName": "Ann Example"})
		default:
			http.Error(w, "no such user", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(Credentials{BaseURL: srv.URL})
	if got, err := c.UserName(context.Background(), "a:1"); err != nil || got != "Ann Example" {
		t.Errorf("UserName = %q, %v", got, err)
	}
	for _, id := range []string{"other", "missing", "blank"} {
		if got, err := c.UserName(context.Background(), id); err == nil {
			t.Errorf("UserName(%s) = %q without an error", id, got)
		}
	}
}
