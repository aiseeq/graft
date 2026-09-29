package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Flags configures graft flags: the project's journal of events that need a
// human look, read through one adapter (sql or http).
type Flags struct {
	DefaultEnv string     `yaml:"default_env"`
	Exceptions string     `yaml:"exceptions"`
	SQL        *FlagsSQL  `yaml:"sql"`
	HTTP       *FlagsHTTP `yaml:"http"`
}

// FlagFields maps graft's event fields to the source's column (sql) or JSON
// field (http) names. id, class, subject, status and last_seen are required.
type FlagFields struct {
	ID        string `yaml:"id"`
	Class     string `yaml:"class"`
	Subject   string `yaml:"subject"`
	Body      string `yaml:"body"`
	Status    string `yaml:"status"`
	Severity  string `yaml:"severity"`
	Key       string `yaml:"key"`
	Times     string `yaml:"times"`
	FirstSeen string `yaml:"first_seen"`
	LastSeen  string `yaml:"last_seen"`
	Note      string `yaml:"note"`
	NotedBy   string `yaml:"noted_by"`
	NotedAt   string `yaml:"noted_at"`
}

// FlagsSQL reads the journal from a PostgreSQL table through psql.
type FlagsSQL struct {
	Table          string                `yaml:"table"`
	Columns        FlagFields            `yaml:"columns"`
	OpenStatuses   []string              `yaml:"open_statuses"`
	ResolvedStatus string                `yaml:"resolved_status"`
	Actor          string                `yaml:"actor"`
	Psql           map[string]EnvCommand `yaml:"psql"`
}

// FlagsHTTP reads the journal from a JSON API.
type FlagsHTTP struct {
	List         HTTPList                  `yaml:"list"`
	Get          HTTPGet                   `yaml:"get"`
	Ack          HTTPAck                   `yaml:"ack"`
	Fields       FlagFields                `yaml:"fields"`
	OpenStatuses []string                  `yaml:"open_statuses"`
	Transport    map[string]*HTTPTransport `yaml:"transport"`
}

// HTTPList is the paged listing of open events.
type HTTPList struct {
	Path        string            `yaml:"path"`
	Query       map[string]string `yaml:"query"`
	Items       string            `yaml:"items"`
	Total       string            `yaml:"total"`
	PageSize    int               `yaml:"page_size"`
	LimitParam  string            `yaml:"limit_param"`
	OffsetParam string            `yaml:"offset_param"`
}

// HTTPGet fetches one event; {id} in the path is the event id.
type HTTPGet struct {
	Path string `yaml:"path"`
	Item string `yaml:"item"`
}

// HTTPAck closes one event; {id}, and {reason} and {actor} in body values, are
// filled in.
type HTTPAck struct {
	Method string            `yaml:"method"`
	Path   string            `yaml:"path"`
	Body   map[string]string `yaml:"body"`
}

// HTTPTransport says how requests reach the API in one environment: directly
// (base_url), or through a command whose stdout is the response body.
type HTTPTransport struct {
	BaseURL string            `yaml:"base_url"`
	Headers map[string]string `yaml:"headers"`
	// Token is run on this machine; its trimmed output fills {token} in headers.
	Token EnvCommand `yaml:"token"`
	// Command runs in the environment with {method}, {path} and {body_b64}
	// filled in, shell-quoted.
	Command string `yaml:"command"`
	// StdinFile is fed to Command, relative to the work tree root.
	StdinFile string `yaml:"stdin_file"`
}

var (
	sqlIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	sqlTableRe = regexp.MustCompile(`^([a-z_][a-z0-9_]*\.)?[a-z_][a-z0-9_]*$`)
)

func (c *Config) validateFlags() error {
	f := c.Flags
	if f == nil {
		return nil
	}
	if f.DefaultEnv == "" {
		return errors.New("flags.default_env: required")
	}
	if err := c.RequireEnv("flags.default_env", f.DefaultEnv); err != nil {
		return err
	}
	if err := checkRelPath("flags.exceptions", f.Exceptions); err != nil {
		return err
	}
	switch {
	case f.SQL != nil && f.HTTP != nil:
		return errors.New("flags: set either sql or http, not both")
	case f.SQL != nil:
		return c.validateFlagsSQL(f.SQL)
	case f.HTTP != nil:
		return c.validateFlagsHTTP(f.HTTP)
	default:
		return errors.New("flags: one adapter is required, sql or http")
	}
}

func (c *Config) validateFlagsSQL(s *FlagsSQL) error {
	if !sqlTableRe.MatchString(s.Table) {
		return fmt.Errorf("flags.sql.table: %q is not a plain table name", s.Table)
	}
	if err := s.Columns.validate("flags.sql.columns", func(v string) bool { return sqlIdentRe.MatchString(v) }); err != nil {
		return err
	}
	if s.Columns.Note == "" {
		return errors.New("flags.sql.columns.note: required, the ack reason is stored there")
	}
	if len(s.OpenStatuses) == 0 || s.ResolvedStatus == "" {
		return errors.New("flags.sql: open_statuses and resolved_status are required")
	}
	if s.Actor == "" {
		s.Actor = "graft"
	}
	return c.validatePerEnv("flags.sql.psql", keysOf(s.Psql), func(env string) error {
		if !s.Psql[env].IsSet() {
			return fmt.Errorf("flags.sql.psql.%s: empty command", env)
		}
		return c.checkRemoteArgv("flags.sql.psql."+env, env, s.Psql[env])
	})
}

func (c *Config) validateFlagsHTTP(h *FlagsHTTP) error {
	if h.List.Path == "" || h.List.Items == "" || h.Get.Path == "" || h.Ack.Path == "" || h.Ack.Method == "" {
		return errors.New("flags.http: list.path, list.items, get.path, ack.method and ack.path are required")
	}
	if h.List.PageSize <= 0 {
		return errors.New("flags.http.list.page_size: required and positive")
	}
	if h.List.LimitParam == "" {
		h.List.LimitParam = "limit"
	}
	if h.List.OffsetParam == "" {
		h.List.OffsetParam = "offset"
	}
	if err := h.Fields.validate("flags.http.fields", func(v string) bool { return v != "" }); err != nil {
		return err
	}
	if len(h.OpenStatuses) == 0 {
		return errors.New("flags.http.open_statuses: required")
	}
	return c.validatePerEnv("flags.http.transport", keysOf(h.Transport), func(env string) error {
		t := h.Transport[env]
		switch {
		case t == nil || (t.BaseURL == "") == (t.Command == ""):
			return fmt.Errorf("flags.http.transport.%s: set exactly one of base_url and command", env)
		case t.Command != "" && (len(t.Headers) > 0 || t.Token.IsSet()):
			return fmt.Errorf("flags.http.transport.%s: headers and token apply to base_url only", env)
		case t.StdinFile != "":
			return checkRelPath(fmt.Sprintf("flags.http.transport.%s.stdin_file", env), t.StdinFile)
		}
		return nil
	})
}

// validatePerEnv checks a per-environment map: every key is a configured env,
// and the default env has an entry.
func (c *Config) validatePerEnv(where string, envs []string, check func(string) error) error {
	if len(envs) == 0 {
		return fmt.Errorf("%s: at least one environment is required", where)
	}
	for _, env := range envs {
		if err := c.RequireEnv(where, env); err != nil {
			return err
		}
		if err := check(env); err != nil {
			return err
		}
	}
	return nil
}

func (f FlagFields) validate(where string, valid func(string) bool) error {
	required := map[string]string{"id": f.ID, "class": f.Class, "subject": f.Subject, "status": f.Status, "last_seen": f.LastSeen}
	for name, v := range required {
		if v == "" {
			return fmt.Errorf("%s.%s: required", where, name)
		}
	}
	all := map[string]string{
		"id": f.ID, "class": f.Class, "subject": f.Subject, "body": f.Body, "status": f.Status,
		"severity": f.Severity, "key": f.Key, "times": f.Times, "first_seen": f.FirstSeen,
		"last_seen": f.LastSeen, "note": f.Note, "noted_by": f.NotedBy, "noted_at": f.NotedAt,
	}
	for name, v := range all {
		if v != "" && !valid(v) {
			return fmt.Errorf("%s.%s: invalid name %q", where, name, v)
		}
	}
	return nil
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
