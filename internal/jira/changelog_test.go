package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
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
