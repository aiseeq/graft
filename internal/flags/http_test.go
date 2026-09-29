package flags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aiseeq/graft/internal/config"
)

// fakeJournalAPI mimics an admin API: wrapped responses, paging, a status
// endpoint that overwrites notes without checking, cookie auth.
type fakeJournalAPI struct {
	mu    sync.Mutex
	items []map[string]any
	pages int
	puts  []string
}

func newFakeAPI() *fakeJournalAPI {
	api := &fakeJournalAPI{}
	for i := 1; i <= 5; i++ {
		api.items = append(api.items, map[string]any{
			"id": fmt.Sprintf("e%d", i), "alertType": "job_stalled", "subject": fmt.Sprintf("job %d stalled", i),
			"body": "details", "status": "new", "severity": "warning", "timesSeen": float64(i),
			"firstSeenAt": "2026-09-01T10:00:00Z", "lastSeenAt": "2026-09-02T10:00:00Z",
		})
	}
	api.items[4]["status"] = "resolved" // listed by a sloppy API, must be ignored
	return api
}

func (f *fakeJournalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, err := r.Cookie("admin_token"); err != nil || c.Value != "tok123" {
		http.Error(w, `{"success":false,"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	wrap := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	const base = "/api/admin/journal"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		f.pages++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		end := min(offset+limit, len(f.items))
		wrap(map[string]any{"items": f.items[offset:end], "total": len(f.items)})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/"):
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		for _, it := range f.items {
			if it["id"] == id {
				wrap(it)
				return
			}
		}
		http.Error(w, `{"success":false,"error":"not found"}`, http.StatusNotFound)
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/status"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/"), "/status")
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["note"] == "" {
			http.Error(w, `{"success":false,"error":"note is required to close"}`, http.StatusBadRequest)
			return
		}
		for _, it := range f.items {
			if it["id"] == id {
				it["status"], it["note"] = body["status"], body["note"]
				f.puts = append(f.puts, id)
				wrap(it)
				return
			}
		}
		http.Error(w, "{}", http.StatusNotFound)
	default:
		http.Error(w, "{}", http.StatusNotFound)
	}
}

func httpConfig(transport *config.HTTPTransport) *config.FlagsHTTP {
	return &config.FlagsHTTP{
		List:         config.HTTPList{Path: "/api/admin/journal", Query: map[string]string{"status": "open"}, Items: "data.items", Total: "data.total", PageSize: 2, LimitParam: "limit", OffsetParam: "offset"},
		Get:          config.HTTPGet{Path: "/api/admin/journal/{id}", Item: "data"},
		Ack:          config.HTTPAck{Method: "PUT", Path: "/api/admin/journal/{id}/status", Body: map[string]string{"status": "resolved", "note": "{reason}"}},
		Fields:       config.FlagFields{ID: "id", Class: "alertType", Subject: "subject", Body: "body", Status: "status", Severity: "severity", Times: "timesSeen", FirstSeen: "firstSeenAt", LastSeen: "lastSeenAt", Note: "note"},
		OpenStatuses: []string{"new", "in_progress"},
		Transport:    map[string]*config.HTTPTransport{"local": transport},
	}
}

func TestHTTPSourceDirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("token command uses echo")
	}
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()
	// The token command gets the listed .env keys and no others.
	cfg := httpConfig(&config.HTTPTransport{
		BaseURL: srv.URL, Headers: map[string]string{"Cookie": "admin_token={token}"},
		Token: config.EnvCommand{Shell: `echo "$ADMIN_TOKEN${OTHER:-}"`},
		Keys:  []config.DotEnvKey{{Name: "ADMIN_TOKEN", NonEmpty: true}},
	})
	env := localEnv(t)
	if err := os.WriteFile(filepath.Join(env.Root, ".env"), []byte("ADMIN_TOKEN=tok123\nOTHER=leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := NewHTTPSource(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	open, err := src.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 4 || api.pages != 3 {
		t.Errorf("open = %d events over %d pages, want 4 over 3", len(open), api.pages)
	}
	if open[1].Times != 2 || open[1].Class != "job_stalled" || open[1].LastSeen == "" {
		t.Errorf("fields: %+v", open[1])
	}
	if err := src.Resolve(ctx, "e1", "handled"); err != nil {
		t.Fatal(err)
	}
	if err := src.Resolve(ctx, "e1", "again"); !errors.Is(err, ErrNotOpen) {
		t.Errorf("closing a closed event: %v", err)
	}
	if len(api.puts) != 1 {
		t.Errorf("PUTs = %v: a closed event must not be overwritten", api.puts)
	}
	if err := src.Resolve(ctx, "e2", ""); err == nil || !strings.Contains(err.Error(), "note is required") {
		t.Errorf("API error body must reach the user: %v", err)
	}
	if _, err := src.Get(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing event: %v", err)
	}
}

func TestHTTPSourceThroughCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	env := localEnv(t)
	// Stands in for a script on a server that holds the credentials: it
	// reads the request from the environment and prints the response.
	script := `case "$P" in
  /api/admin/journal\?*) echo '{"data":{"items":[{"id":"r1","alertType":"x","subject":"s","status":"new","lastSeenAt":"2026-09-02T10:00:00Z"}],"total":1}}' ;;
  /api/admin/journal/r1) echo '{"data":{"id":"r1","alertType":"x","subject":"s","status":"new"}}' ;;
  /api/admin/journal/r1/status) printf '%s' "$B" | base64 -d > "$OUT"; echo "{\"method\":\"$M\"}" ;;
  *) echo "unexpected $P" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(env.Root, "api.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(env.Root, "put.json")
	cfg := httpConfig(&config.HTTPTransport{
		Command:   "OUT=" + out + " M={method} P={path} B={body_b64} sh -s",
		StdinFile: "api.sh",
	})
	src, err := NewHTTPSource(cfg, env)
	if err != nil {
		t.Fatal(err)
	}
	open, err := src.Open(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("open: %v %v", open, err)
	}
	if err := src.Resolve(context.Background(), "r1", `it's "fine"`); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(out)
	if string(body) != `{"note":"it's \"fine\"","status":"resolved"}` {
		t.Errorf("request body = %s", body)
	}
}

func TestStampIsUTCWithZone(t *testing.T) {
	for in, want := range map[string]string{
		"2026-09-02T12:00:00+02:00": "2026-09-02 10:00 UTC",
		"2026-09-02T10:00:00.5Z":    "2026-09-02 10:00 UTC",
		"":                          "",
	} {
		if got, err := stamp("t", in); err != nil || got != want {
			t.Errorf("stamp(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
