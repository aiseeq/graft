// Package jira posts release notes to Jira work items: a comment on every
// item a deploy delivered, and optionally a move to a status. Nothing here
// can fail a deploy; the release is already live when it runs.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/dotenv"
)

// Credentials are read from the user's env file.
type Credentials struct {
	BaseURL string
	Email   string
	Token   string
}

// LoadCredentials reads JIRA_BASE_URL, JIRA_EMAIL and JIRA_API_TOKEN.
func LoadCredentials(envFile string) (Credentials, error) {
	f, err := dotenv.Load(envFile)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	var missing []string
	for key, into := range map[string]*string{"JIRA_BASE_URL": &c.BaseURL, "JIRA_EMAIL": &c.Email, "JIRA_API_TOKEN": &c.Token} {
		v, ok := f.Get(key)
		if !ok || v == "" {
			missing = append(missing, key)
		}
		*into = v
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return Credentials{}, fmt.Errorf("%s: missing %s", envFile, strings.Join(missing, ", "))
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

// Client talks to the Jira REST API v3.
type Client struct {
	creds Credentials
	http  *http.Client
}

// New returns a client with a per-request timeout.
func New(creds Credentials) *Client {
	return &Client{creds: creds, http: &http.Client{Timeout: 15 * time.Second}}
}

// Comment posts a plain-text comment to an item.
func (c *Client) Comment(ctx context.Context, key, text string) error {
	body := map[string]any{"body": map[string]any{
		"type": "doc", "version": 1,
		"content": []any{map[string]any{
			"type":    "paragraph",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}},
	}}
	_, err := c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+key+"/comment", body, http.StatusCreated)
	return err
}

// Transition moves an item to the status named to, unless it is already
// there or in one of skip. It reports whether the item moved.
func (c *Client) Transition(ctx context.Context, key, to string, skip []string) (bool, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+key+"?fields=status&expand=transitions", nil, http.StatusOK)
	if err != nil {
		return false, err
	}
	var issue struct {
		Fields struct {
			Status struct {
				Name string `json:"name"`
			} `json:"status"`
		} `json:"fields"`
		Transitions []struct {
			ID string `json:"id"`
			To struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(data, &issue); err != nil {
		return false, fmt.Errorf("%s: unexpected issue JSON: %w", key, err)
	}
	status := issue.Fields.Status.Name
	if status == to || slices.Contains(skip, status) {
		return false, nil
	}
	for _, t := range issue.Transitions {
		if t.To.Name == to {
			body := map[string]any{"transition": map[string]string{"id": t.ID}}
			_, err := c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+key+"/transitions", body, http.StatusNoContent)
			return err == nil, err
		}
	}
	return false, fmt.Errorf("%s: no transition from %q to %q", key, status, to)
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.creds.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.creds.Email, c.creds.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	return data, nil
}
