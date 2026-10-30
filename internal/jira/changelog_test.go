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
			"isLast": start == 1,
			"values": []any{map[string]any{"items": []any{item}}},
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
				_ = json.NewEncoder(w).Encode(map[string]any{"isLast": true, "values": tc.entries})
			}))
			defer srv.Close()
			got, _, err := New(Credentials{BaseURL: srv.URL}).FixVersionHistoryAfter(context.Background(), "PROJ-1", "10010", at)
			if err != nil || got != tc.want {
				t.Errorf("FixVersionHistoryAfter = %v, %v; want %v", got, err, tc.want)
			}
		})
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
