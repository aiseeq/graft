package flags

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/tasks"
)

// maxPages bounds the listing loop against an API that never reports the end.
const maxPages = 1000

// HTTPSource reads the journal from a JSON API, either directly or through a
// command run in the environment (an ssh hop to a script that holds the
// credentials).
type HTTPSource struct {
	cfg       *config.FlagsHTTP
	env       *envs.Env
	transport *config.HTTPTransport
	client    *http.Client
	token     string
	tokenRead bool
}

// NewHTTPSource binds the adapter to an environment.
func NewHTTPSource(cfg *config.FlagsHTTP, env *envs.Env) (*HTTPSource, error) {
	t, ok := cfg.Transport[env.Name]
	if !ok {
		return nil, fmt.Errorf("flags.http.transport has no entry for env %s", env.Name)
	}
	return &HTTPSource{cfg: cfg, env: env, transport: t, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Ready is always true: an API that answers has a journal.
func (s *HTTPSource) Ready(context.Context) (bool, error) { return true, nil }

// Open pages through the listing until the reported total (or a short page).
func (s *HTTPSource) Open(ctx context.Context) ([]Event, error) {
	l := s.cfg.List
	var events []Event
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		for k, v := range l.Query {
			q.Set(k, v)
		}
		q.Set(l.LimitParam, strconv.Itoa(l.PageSize))
		q.Set(l.OffsetParam, strconv.Itoa(len(events)))
		body, err := s.request(ctx, http.MethodGet, l.Path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		doc, err := decode(body)
		if err != nil {
			return nil, err
		}
		items, err := lookupPath(doc, l.Items)
		if err != nil {
			return nil, err
		}
		list, ok := items.([]any)
		if !ok {
			return nil, fmt.Errorf("list: %s is %T, not an array", l.Items, items)
		}
		for _, item := range list {
			e, err := s.event(item)
			if err != nil {
				return nil, err
			}
			if slices.Contains(s.cfg.OpenStatuses, e.Status) {
				events = append(events, e)
			}
		}
		done, err := s.lastPage(doc, len(list), page)
		if err != nil || done {
			return events, err
		}
	}
	return nil, fmt.Errorf("list: still not at the end after %d pages", maxPages)
}

func (s *HTTPSource) lastPage(doc any, pageLen, page int) (bool, error) {
	l := s.cfg.List
	if pageLen < l.PageSize {
		return true, nil
	}
	if l.Total == "" {
		return false, nil
	}
	raw, err := lookupPath(doc, l.Total)
	if err != nil {
		return false, err
	}
	total, ok := raw.(float64)
	if !ok {
		return false, fmt.Errorf("list: %s is %T, not a number", l.Total, raw)
	}
	return (page+1)*l.PageSize >= int(total), nil
}

// Get returns one event.
func (s *HTTPSource) Get(ctx context.Context, id string) (Event, error) {
	if err := checkID(id); err != nil {
		return Event{}, err
	}
	path, err := envs.Fill(s.cfg.Get.Path, map[string]string{"id": id}, url.PathEscape)
	if err != nil {
		return Event{}, err
	}
	body, err := s.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Event{}, err
	}
	doc, err := decode(body)
	if err != nil {
		return Event{}, err
	}
	item := doc
	if s.cfg.Get.Item != "" {
		if item, err = lookupPath(doc, s.cfg.Get.Item); err != nil {
			return Event{}, err
		}
	}
	return s.event(item)
}

// Resolve checks that the event is open, then closes it: the API itself may
// overwrite a closed event's note without complaint.
func (s *HTTPSource) Resolve(ctx context.Context, id, reason string) error {
	e, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !slices.Contains(s.cfg.OpenStatuses, e.Status) {
		return fmt.Errorf("event %s is %s: %w", id, e.Status, ErrNotOpen)
	}
	path, err := envs.Fill(s.cfg.Ack.Path, map[string]string{"id": id}, url.PathEscape)
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for k, v := range s.cfg.Ack.Body {
		filled, err := envs.Fill(v, map[string]string{"reason": reason, "id": id}, func(s string) string { return s })
		if err != nil {
			return err
		}
		fields[k] = filled
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding the ack body of %s: %w", id, err)
	}
	_, err = s.request(ctx, s.cfg.Ack.Method, path, payload)
	return err
}

func (s *HTTPSource) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if s.transport.Command != "" {
		return s.viaCommand(ctx, method, path, body)
	}
	return s.direct(ctx, method, path, body)
}

func (s *HTTPSource) direct(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.transport.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range s.transport.Headers {
		value, err := s.headerValue(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		req.Header.Set(k, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the response: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, truncate(strings.TrimSpace(string(data)), 500))
	}
	return data, nil
}

// headerValue fills {token} and ${KEY}.
func (s *HTTPSource) headerValue(ctx context.Context, v string) (string, error) {
	if strings.Contains(v, "{token}") {
		token, err := s.readToken(ctx)
		if err != nil {
			return "", err
		}
		v = strings.ReplaceAll(v, "{token}", token)
	}
	return dotenv.Expand(v, s.env.Lookup)
}

func (s *HTTPSource) readToken(ctx context.Context) (string, error) {
	if s.tokenRead {
		return s.token, nil
	}
	if !s.transport.Token.IsSet() {
		return "", fmt.Errorf("{token} is used but flags.http.transport.%s.token is not set", s.env.Name)
	}
	local := &envs.Env{Name: "local", Root: s.env.Root, Lookup: s.env.Lookup}
	cmd, err := local.Command(ctx, s.transport.Token)
	if err != nil {
		return "", err
	}
	keys, err := tasks.KeyValues(s.env.Lookup, s.transport.Keys)
	if err != nil {
		return "", fmt.Errorf("token command: %w", err)
	}
	cmd.Env = append(os.Environ(), keys...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("token command: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	s.token, s.tokenRead = strings.TrimSpace(string(out)), true
	if s.token == "" {
		return "", fmt.Errorf("token command printed nothing")
	}
	return s.token, nil
}

func (s *HTTPSource) viaCommand(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	line, err := envs.Fill(s.transport.Command, map[string]string{
		"method":   method,
		"path":     path,
		"body_b64": base64.StdEncoding.EncodeToString(body),
	}, envs.Quote)
	if err != nil {
		return nil, err
	}
	cmd := s.env.Shell(ctx, line)
	if s.transport.StdinFile != "" {
		f, err := os.Open(filepath.Join(s.env.Root, filepath.FromSlash(s.transport.StdinFile)))
		if err != nil {
			return nil, fmt.Errorf("flags.http.transport.%s.stdin_file: %w", s.env.Name, err)
		}
		defer f.Close()
		cmd.Stdin = f
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s via %s: %w: %s", method, path, s.env.Name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func decode(body []byte) (any, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("response is not JSON: %q", truncate(strings.TrimSpace(string(body)), 200))
	}
	return doc, nil
}

// lookupPath follows a dotted path of object keys.
func lookupPath(doc any, path string) (any, error) {
	cur := doc
	for key := range strings.SplitSeq(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("response: %s: %q is not inside an object", path, key)
		}
		if cur, ok = obj[key]; !ok {
			return nil, fmt.Errorf("response: %s: no field %q", path, key)
		}
	}
	return cur, nil
}

func (s *HTTPSource) event(item any) (Event, error) {
	obj, ok := item.(map[string]any)
	if !ok {
		return Event{}, fmt.Errorf("event is %T, not an object", item)
	}
	f := s.cfg.Fields
	str := func(name string) string {
		if name == "" {
			return ""
		}
		switch v := obj[name].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		default:
			return ""
		}
	}
	e := Event{
		ID: str(f.ID), Class: str(f.Class), Subject: str(f.Subject), Body: str(f.Body),
		Status: str(f.Status), Severity: str(f.Severity), Key: str(f.Key),
		Note: str(f.Note), NotedBy: str(f.NotedBy),
	}
	for _, t := range []struct {
		field string
		into  *string
	}{{f.FirstSeen, &e.FirstSeen}, {f.LastSeen, &e.LastSeen}, {f.NotedAt, &e.NotedAt}} {
		v, err := stamp(t.field, str(t.field))
		if err != nil {
			return Event{}, err
		}
		*t.into = v
	}
	if f.Times != "" {
		if n, ok := obj[f.Times].(float64); ok {
			e.Times = int(n)
		}
	}
	if e.ID == "" {
		return Event{}, fmt.Errorf("event without %s: %v", f.ID, obj)
	}
	return e, nil
}

// stamp renders an RFC 3339 time the way the SQL adapter does: in UTC, with
// the zone named. An absent field stays empty; anything else that is not
// RFC 3339 is an error.
func stamp(field, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", fmt.Errorf("field %s: %q is not an RFC 3339 time", field, s)
	}
	return t.UTC().Format("2006-01-02 15:04") + " UTC", nil
}
