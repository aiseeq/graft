package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFindPageTakesTheExactTitleOnly: the title filter of the search also
// returns pages whose title only contains the one asked for.
func TestFindPageTakesTheExactTitleOnly(t *testing.T) {
	results := []any{
		map[string]any{"id": "1", "title": "Release 1.2.0 notes"},
		map[string]any{"id": "2", "title": "Release 1.2.0", "version": map[string]int{"number": 3}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wiki/api/v2/pages" || r.URL.Query().Get("space-id") != "S1" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()
	c := New(Credentials{BaseURL: srv.URL})

	page, ok, err := c.FindPage(context.Background(), "S1", "Release 1.2.0")
	if err != nil || !ok || page.ID != "2" || page.Version != 3 {
		t.Errorf("FindPage = %+v %v %v, want page 2", page, ok, err)
	}
	if page, ok, err := c.FindPage(context.Background(), "S1", "Release 1.2"); err != nil || ok {
		t.Errorf("FindPage of a title no page has = %+v %v %v, want none", page, ok, err)
	}
}
