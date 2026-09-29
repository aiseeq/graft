package flags

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/envs"
)

// These tests run the SQL adapter against a real PostgreSQL: the queries are
// the product. make pg-up starts one and exports GRAFT_TEST_PG_DSN.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GRAFT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("GRAFT_TEST_PG_DSN is not set: run make pg-up, or make test")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql is not installed")
	}
	return dsn
}

func psqlExec(t *testing.T, dsn, sql string) {
	t.Helper()
	cmd := exec.Command("psql", dsn, "-X", "-q", "-v", "ON_ERROR_STOP=1")
	cmd.Stdin = strings.NewReader(sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("psql: %v\n%s", err, out)
	}
}

// newSchema creates a throwaway schema for one test.
func newSchema(t *testing.T, dsn string) string {
	t.Helper()
	schema := fmt.Sprintf("t%d", time.Now().UnixNano())
	psqlExec(t, dsn, "CREATE SCHEMA "+schema+";")
	t.Cleanup(func() { psqlExec(t, dsn, "DROP SCHEMA "+schema+" CASCADE;") })
	return schema
}

var psqlArgv = []string{"psql", "${GRAFT_TEST_PG_DSN}", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1"}

func localEnv(t *testing.T) *envs.Env {
	root := t.TempDir()
	return &envs.Env{Name: "local", Root: root, Lookup: dotenv.NewLookup(filepath.Join(root, ".env"))}
}

// ops-style journal: open/resolved statuses, severity, uuid ids.
func opsJournal(t *testing.T, dsn string) (*config.FlagsSQL, string) {
	schema := newSchema(t, dsn)
	psqlExec(t, dsn, `CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE `+schema+`.events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL, object_key text NOT NULL, severity text NOT NULL DEFAULT 'warning',
  subject text NOT NULL, body text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'open',
  note text, noted_by text, noted_at timestamptz, times_seen int NOT NULL DEFAULT 1,
  first_seen_at timestamptz NOT NULL DEFAULT now(), last_seen_at timestamptz NOT NULL DEFAULT now());
INSERT INTO `+schema+`.events (id, kind, object_key, subject, body, times_seen, last_seen_at) VALUES
 ('00000000-0000-0000-0000-000000000001', 'http_failure', 'k1', 'POST /pay: timeout | upstream', 'line one
line two with ''quotes''', 3, now() - interval '1 hour'),
 ('00000000-0000-0000-0000-000000000002', 'log_error', 'k2', 'disk almost full', '', 1, now()),
 ('00000000-0000-0000-0000-000000000003', 'log_error', 'k3', 'already handled', '', 1, now());
UPDATE `+schema+`.events SET status = 'resolved', note = 'old' WHERE object_key = 'k3';`)
	return &config.FlagsSQL{
		Table: schema + ".events",
		Columns: config.FlagFields{ID: "id", Class: "kind", Subject: "subject", Body: "body", Status: "status",
			Severity: "severity", Key: "object_key", Times: "times_seen", FirstSeen: "first_seen_at",
			LastSeen: "last_seen_at", Note: "note", NotedBy: "noted_by", NotedAt: "noted_at"},
		OpenStatuses: []string{"open"}, ResolvedStatus: "resolved", Actor: "agent",
		Psql: map[string]config.EnvCommand{"local": {Argv: psqlArgv}},
	}, schema
}

func TestSQLSourceOpsJournal(t *testing.T) {
	dsn := pgDSN(t)
	cfg, schema := opsJournal(t, dsn)
	src, err := NewSQLSource(cfg, localEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ready, err := src.Ready(ctx); err != nil || !ready {
		t.Fatalf("ready = %v, %v", ready, err)
	}
	open, err := src.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].ID != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("open events, newest first: %+v", open)
	}
	if open[1].Subject != "POST /pay: timeout | upstream" || open[1].Times != 3 || open[1].Severity != "warning" {
		t.Errorf("fields: %+v", open[1])
	}
	e, err := src.Get(ctx, "00000000-0000-0000-0000-000000000001")
	if err != nil || e.Body != "line one\nline two with 'quotes'" {
		t.Errorf("get: %+v %v", e, err)
	}

	if err := src.Resolve(ctx, "00000000-0000-0000-0000-000000000002", "it's fine; was a test"); err != nil {
		t.Fatal(err)
	}
	e, _ = src.Get(ctx, "00000000-0000-0000-0000-000000000002")
	if e.Status != "resolved" || e.Note != "it's fine; was a test" || e.NotedBy != "agent" || e.NotedAt == "" {
		t.Errorf("resolved: %+v", e)
	}
	err = src.Resolve(ctx, "00000000-0000-0000-0000-000000000003", "again")
	if !errors.Is(err, ErrNotOpen) {
		t.Errorf("closing a closed event: %v", err)
	}
	e, _ = src.Get(ctx, "00000000-0000-0000-0000-000000000003")
	if e.Note != "old" {
		t.Errorf("closed event overwritten: %+v", e)
	}
	if _, err := src.Get(ctx, "x' OR '1'='1"); err == nil {
		t.Error("injection-shaped id accepted")
	}

	cfg.Table = schema + ".missing"
	missing, _ := NewSQLSource(cfg, localEnv(t))
	if ready, err := missing.Ready(ctx); err != nil || ready {
		t.Errorf("missing table: ready = %v, %v", ready, err)
	}
}

// notice-style journal: several open statuses, no severity, id text.
func TestSQLSourceNoticesThroughShell(t *testing.T) {
	dsn := pgDSN(t)
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	schema := newSchema(t, dsn)
	psqlExec(t, dsn, `CREATE TABLE `+schema+`.notices (
  id text PRIMARY KEY, notice_type text NOT NULL, subject text NOT NULL, body text NOT NULL,
  status text NOT NULL DEFAULT 'new', note text, noted_by text, noted_at timestamptz,
  times_sent int NOT NULL DEFAULT 1, first_sent_at timestamptz DEFAULT now(), last_sent_at timestamptz DEFAULT now());
INSERT INTO `+schema+`.notices (id, notice_type, subject, body, status) VALUES
 ('a1', 'job_stalled', 'job X stalled', 'b', 'new'),
 ('a2', 'job_stalled', 'job Y stalled', 'b', 'in_progress'),
 ('a3', 'deposit', 'deposit arrived', 'b', 'wontfix');`)
	cfg := &config.FlagsSQL{
		Table: schema + ".notices",
		Columns: config.FlagFields{ID: "id", Class: "notice_type", Subject: "subject", Body: "body", Status: "status",
			Times: "times_sent", FirstSeen: "first_sent_at", LastSeen: "last_sent_at", Note: "note", NotedBy: "noted_by", NotedAt: "noted_at"},
		OpenStatuses: []string{"new", "in_progress"}, ResolvedStatus: "resolved", Actor: "agent",
		// A shell command line, as a remote environment would have.
		Psql: map[string]config.EnvCommand{"local": {Shell: `psql "$GRAFT_TEST_PG_DSN" -X -q -t -A -v ON_ERROR_STOP=1`}},
	}
	src, err := NewSQLSource(cfg, localEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rules := []Rule{{Class: "job_stalled", Substring: "job Y", Reason: "known"}}
	j := &Journal{Env: "local", Source: src, Rules: rules, Out: &out}
	if err := j.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "flag RAISED (local): 1 events to review, 1 muted") || !strings.Contains(out.String(), "a1") || strings.Contains(out.String(), "a2\n") {
		t.Errorf("status:\n%s", out.String())
	}
	if err := j.Ack(context.Background(), "a2", "handled"); err != nil {
		t.Errorf("in_progress counts as open: %v", err)
	}
}

func TestSQLSourceRejectsNonJSONOutput(t *testing.T) {
	dsn := pgDSN(t)
	cfg, _ := opsJournal(t, dsn)
	// Without -t -A psql prints a table: the adapter must not guess.
	cfg.Psql["local"] = config.EnvCommand{Argv: []string{"psql", "${GRAFT_TEST_PG_DSN}", "-X", "-q"}}
	src, _ := NewSQLSource(cfg, localEnv(t))
	if _, err := src.Open(context.Background()); err == nil || !strings.Contains(err.Error(), "did not return JSON") {
		t.Errorf("err = %v", err)
	}
}
