package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// journalServer is a minimal journal API for the CLI tests.
type journalServer struct {
	mu     sync.Mutex
	events map[string]map[string]any
	order  []string
}

func (s *journalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/journal":
		var items []any
		for _, id := range s.order {
			items = append(items, s.events[id])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
	case r.Method == http.MethodGet:
		if e, ok := s.events[strings.TrimPrefix(r.URL.Path, "/journal/")]; ok {
			_ = json.NewEncoder(w).Encode(e)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	case r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/journal/"), "/close")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.events[id]["status"], s.events[id]["note"] = "closed", body["note"]
		_ = json.NewEncoder(w).Encode(s.events[id])
	}
}

func flagsRepo(t *testing.T) (*fixture, *journalServer) {
	api := &journalServer{events: map[string]map[string]any{}}
	for _, e := range []map[string]any{
		{"id": "ev1", "type": "job_stalled", "subject": "job 17 stalled for 2h", "status": "open", "body": "worker log tail"},
		{"id": "ev2", "type": "log_error", "subject": "disk almost full on /var", "status": "open"},
		{"id": "ev3", "type": "log_error", "subject": "known noise", "status": "open"},
	} {
		api.events[e["id"].(string)] = e
		api.order = append(api.order, e["id"].(string))
	}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
envs:
  local: {}
  prod: {ssh: {host: 10.0.0.2}}
flags:
  default_env: local
  exceptions: flags-exceptions.conf
  http:
    list: {path: /journal, items: items, total: total, page_size: 100}
    get: {path: '/journal/{id}'}
    ack: {method: POST, path: '/journal/{id}/close', body: {note: '{reason}'}}
    fields: {id: id, class: type, subject: subject, body: body, status: status, last_seen: seen}
    open_statuses: [open]
    transport:
      local: {base_url: '` + srv.URL + `'}
`,
		"flags-exceptions.conf": "# env|class|substring|reason\n*|log_error|known noise|harmless, logged by the cron wrapper\n",
	})
	return f, api
}

func TestFlagsCommands(t *testing.T) {
	f, api := flagsRepo(t)
	out := f.mustGraft("flags")
	if !strings.Contains(out, "flag RAISED (local): 2 events to review, 1 muted") || !strings.Contains(out, "ev1") || strings.Contains(out, "ev3") {
		t.Errorf("status:\n%s", out)
	}
	if out := f.mustGraft("flags", "show", "ev1"); !strings.Contains(out, "worker log tail") {
		t.Errorf("show:\n%s", out)
	}
	if out, code := f.graft("", "flags", "ack", "ev1"); code == 0 || !strings.Contains(out, "--reason is required") {
		t.Errorf("ack without reason: %d\n%s", code, out)
	}
	// A reason that happens to be an env name stays a reason.
	f.mustGraft("flags", "ack", "ev1", "--reason", "prod")
	api.mu.Lock()
	note := api.events["ev1"]["note"]
	api.mu.Unlock()
	if note != "prod" {
		t.Errorf("note = %v", note)
	}

	out = f.mustGraft("flags", "mute", "ev2", "--match", "disk almost full", "--reason", "alerting covers disks")
	if !strings.Contains(out, "added to") {
		t.Errorf("mute:\n%s", out)
	}
	if got := f.read("flags-exceptions.conf"); !strings.HasSuffix(got, "local|log_error|disk almost full|alerting covers disks\n") {
		t.Errorf("exceptions file:\n%s", got)
	}
	if out := f.mustGraft("flags", "--env", "local"); !strings.Contains(out, "flag down (local): nothing to review") {
		t.Errorf("after mute:\n%s", out)
	}
	if out, code := f.graft("", "flags", "mute", "ev3", "--match", "not in subject", "--reason", "x"); code == 0 {
		t.Errorf("--match outside the subject accepted:\n%s", out)
	}
	if out, code := f.graft("", "flags", "--env", "staging"); code == 0 || !strings.Contains(out, "unknown env") {
		t.Errorf("unknown env: %d\n%s", code, out)
	}
}

func TestCheckValidatesFlagExceptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not needed twice")
	}
	f, _ := flagsRepo(t)
	f.git("add", "-A")
	f.mustGraft("check")
	f.write("flags-exceptions.conf", "log_error|no env column|reason\n")
	f.git("add", "-A")
	out, code := f.graft("", "check")
	if code == 0 || !strings.Contains(out, "flags-exceptions.conf") || !strings.Contains(out, "want env|class|substring|reason") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
