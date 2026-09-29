package flags

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/envs"
)

// SQLSource reads the journal from a PostgreSQL table through a psql process
// that takes SQL on stdin. Every query returns a single JSON value, so
// multi-line bodies and | in subjects need no delimiter tricks, and output
// that is not JSON (psql without ON_ERROR_STOP swallowing an error) is caught.
type SQLSource struct {
	cfg  *config.FlagsSQL
	env  *envs.Env
	psql config.EnvCommand
}

// NewSQLSource binds the adapter to an environment.
func NewSQLSource(cfg *config.FlagsSQL, env *envs.Env) (*SQLSource, error) {
	psql, ok := cfg.Psql[env.Name]
	if !ok {
		return nil, fmt.Errorf("flags.sql.psql has no command for env %s", env.Name)
	}
	return &SQLSource{cfg: cfg, env: env, psql: psql}, nil
}

const timeFormat = "YYYY-MM-DD HH24:MI"

func ident(name string) string { return `"` + name + `"` }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (s *SQLSource) table() string {
	schema, name, found := strings.Cut(s.cfg.Table, ".")
	if found {
		return ident(schema) + "." + ident(name)
	}
	return ident(schema)
}

// eventJSON builds json_build_object over the mapped columns.
func (s *SQLSource) eventJSON(withBody bool) string {
	c := s.cfg.Columns
	var parts []string
	add := func(key, expr string) {
		parts = append(parts, literal(key), expr)
	}
	text := func(key, col string) {
		if col != "" {
			add(key, ident(col)+"::text")
		}
	}
	stamp := func(key, col string) {
		if col != "" {
			add(key, "to_char("+ident(col)+", "+literal(timeFormat)+")")
		}
	}
	text("id", c.ID)
	text("class", c.Class)
	text("subject", c.Subject)
	text("status", c.Status)
	text("severity", c.Severity)
	text("key", c.Key)
	if c.Times != "" {
		add("times", ident(c.Times)+"::bigint")
	}
	stamp("first_seen", c.FirstSeen)
	stamp("last_seen", c.LastSeen)
	if withBody {
		text("body", c.Body)
		text("note", c.Note)
		text("noted_by", c.NotedBy)
		stamp("noted_at", c.NotedAt)
	}
	return "json_build_object(" + strings.Join(parts, ", ") + ")"
}

func (s *SQLSource) openCondition() string {
	statuses := make([]string, len(s.cfg.OpenStatuses))
	for i, st := range s.cfg.OpenStatuses {
		statuses[i] = literal(st)
	}
	return ident(s.cfg.Columns.Status) + " IN (" + strings.Join(statuses, ", ") + ")"
}

// Ready checks that the table exists.
func (s *SQLSource) Ready(ctx context.Context) (bool, error) {
	var out struct {
		Ready bool `json:"ready"`
	}
	q := "SELECT json_build_object('ready', to_regclass(" + literal(s.cfg.Table) + ") IS NOT NULL);"
	if err := s.query(ctx, q, &out); err != nil {
		return false, err
	}
	return out.Ready, nil
}

// Open lists open events, most recently seen first.
func (s *SQLSource) Open(ctx context.Context) ([]Event, error) {
	q := "SELECT coalesce(json_agg(" + s.eventJSON(false) + " ORDER BY " + ident(s.cfg.Columns.LastSeen) +
		" DESC), '[]'::json) FROM " + s.table() + " WHERE " + s.openCondition() + ";"
	var rows []jsonEvent
	if err := s.query(ctx, q, &rows); err != nil {
		return nil, err
	}
	return toEvents(rows), nil
}

// Get returns one event, open or not.
func (s *SQLSource) Get(ctx context.Context, id string) (Event, error) {
	if err := checkID(id); err != nil {
		return Event{}, err
	}
	q := "SELECT coalesce(json_agg(" + s.eventJSON(true) + "), '[]'::json) FROM " + s.table() +
		" WHERE " + ident(s.cfg.Columns.ID) + "::text = " + literal(id) + ";"
	var rows []jsonEvent
	if err := s.query(ctx, q, &rows); err != nil {
		return Event{}, err
	}
	if len(rows) != 1 {
		return Event{}, fmt.Errorf("event %s not found", id)
	}
	return rows[0].event(), nil
}

// Resolve closes an open event; a missing or closed one is ErrNotOpen.
func (s *SQLSource) Resolve(ctx context.Context, id, reason string) error {
	if err := checkID(id); err != nil {
		return err
	}
	c := s.cfg.Columns
	set := []string{
		ident(c.Status) + " = " + literal(s.cfg.ResolvedStatus),
		ident(c.Note) + " = " + literal(reason),
	}
	if c.NotedBy != "" {
		set = append(set, ident(c.NotedBy)+" = "+literal(s.cfg.Actor))
	}
	if c.NotedAt != "" {
		set = append(set, ident(c.NotedAt)+" = now()")
	}
	q := "WITH u AS (UPDATE " + s.table() + " SET " + strings.Join(set, ", ") +
		" WHERE " + ident(c.ID) + "::text = " + literal(id) + " AND " + s.openCondition() +
		" RETURNING 1) SELECT json_build_object('updated', count(*)) FROM u;"
	var out struct {
		Updated int `json:"updated"`
	}
	if err := s.query(ctx, q, &out); err != nil {
		return err
	}
	if out.Updated != 1 {
		return fmt.Errorf("event %s: %w", id, ErrNotOpen)
	}
	return nil
}

func (s *SQLSource) query(ctx context.Context, sql string, into any) error {
	cmd, err := s.env.Command(ctx, s.psql)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql (%s): %w: %s", s.env.Name, err, strings.TrimSpace(stderr.String()))
	}
	out := bytes.TrimSpace(stdout.Bytes())
	if err := json.Unmarshal(out, into); err != nil {
		return fmt.Errorf("psql (%s) did not return JSON (the command needs -X -q -t -A -v ON_ERROR_STOP=1): %q %s",
			s.env.Name, truncate(string(out), 200), strings.TrimSpace(stderr.String()))
	}
	return nil
}

// jsonEvent is an event as the adapters decode it.
type jsonEvent struct {
	ID        string `json:"id"`
	Class     string `json:"class"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	Severity  string `json:"severity"`
	Key       string `json:"key"`
	Times     int    `json:"times"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Note      string `json:"note"`
	NotedBy   string `json:"noted_by"`
	NotedAt   string `json:"noted_at"`
}

func (j jsonEvent) event() Event {
	return Event(j)
}

func toEvents(rows []jsonEvent) []Event {
	events := make([]Event, len(rows))
	for i, r := range rows {
		events[i] = r.event()
	}
	return events
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
