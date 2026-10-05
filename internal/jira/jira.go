// Package jira posts release notes to Jira work items: a comment on every
// open item a deploy delivered that is or was assigned to the token's user,
// and optionally a move to a status. Nothing here can fail a deploy; the release
// is already live when it runs.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Myself returns the account id of the user the token belongs to.
func (c *Client) Myself(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/myself", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var me struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(data, &me); err != nil {
		return "", fmt.Errorf("unexpected /myself JSON: %w", err)
	}
	if me.AccountID == "" {
		return "", errors.New("/myself returned no accountId")
	}
	return me.AccountID, nil
}

// Issue is what release notes need to know about an item.
type Issue struct {
	Key    string
	Status string
	// Done is true in a status of the done category (Done, Won't do).
	Done bool
	// AssigneeID and AssigneeName are empty for an unassigned item.
	AssigneeID   string
	AssigneeName string
	Transitions  []Transition
}

// Transition is a move available from the item's current status.
type Transition struct {
	ID string
	To string
}

// Issue reads an item's status, assignee and available transitions.
func (c *Client) Issue(ctx context.Context, key string) (Issue, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/issue/"+key+"?fields=status,assignee&expand=transitions", nil, http.StatusOK)
	if err != nil {
		return Issue{}, err
	}
	var raw struct {
		Fields struct {
			Status struct {
				Name     string `json:"name"`
				Category struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"status"`
			Assignee *struct {
				AccountID   string `json:"accountId"`
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
		} `json:"fields"`
		Transitions []struct {
			ID string `json:"id"`
			To struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Issue{}, fmt.Errorf("%s: unexpected issue JSON: %w", key, err)
	}
	if raw.Fields.Status.Category.Key == "" {
		return Issue{}, fmt.Errorf("%s: the status has no category", key)
	}
	issue := Issue{Key: key, Status: raw.Fields.Status.Name, Done: raw.Fields.Status.Category.Key == "done"}
	if a := raw.Fields.Assignee; a != nil {
		if a.AccountID == "" {
			return Issue{}, fmt.Errorf("%s: the assignee has no accountId", key)
		}
		issue.AssigneeID, issue.AssigneeName = a.AccountID, a.DisplayName
	}
	for _, t := range raw.Transitions {
		issue.Transitions = append(issue.Transitions, Transition{ID: t.ID, To: t.To.Name})
	}
	return issue, nil
}

// changelogPage is how many history entries one request reads.
const changelogPage = 100

// WasAssignedTo reports whether the item's history shows it assigned to the
// account at some point: handed over from it or to it.
func (c *Client) WasAssignedTo(ctx context.Context, key, accountID string) (bool, error) {
	for start := 0; ; {
		data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=%d", key, start, changelogPage), nil, http.StatusOK)
		if err != nil {
			return false, err
		}
		var page struct {
			IsLast bool `json:"isLast"`
			Values []struct {
				Items []struct {
					FieldID string  `json:"fieldId"`
					From    *string `json:"from"`
					To      *string `json:"to"`
				} `json:"items"`
			} `json:"values"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return false, fmt.Errorf("%s: unexpected changelog JSON: %w", key, err)
		}
		for _, v := range page.Values {
			for _, it := range v.Items {
				if it.FieldID != "assignee" {
					continue
				}
				if (it.From != nil && *it.From == accountID) || (it.To != nil && *it.To == accountID) {
					return true, nil
				}
			}
		}
		if page.IsLast {
			return false, nil
		}
		if len(page.Values) == 0 {
			return false, fmt.Errorf("%s: changelog page at %d is empty but not the last", key, start)
		}
		start += len(page.Values)
	}
}

// Transition moves an item to the status named to, unless it is already
// there or in one of skip. It reports whether the item moved.
func (c *Client) Transition(ctx context.Context, issue Issue, to string, skip []string) (bool, error) {
	if issue.Status == to || slices.Contains(skip, issue.Status) {
		return false, nil
	}
	for _, t := range issue.Transitions {
		if t.To == to {
			body := map[string]any{"transition": map[string]string{"id": t.ID}}
			_, err := c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+issue.Key+"/transitions", body, http.StatusNoContent)
			return err == nil, err
		}
	}
	return false, fmt.Errorf("%s: no transition from %q to %q", issue.Key, issue.Status, to)
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding the %s %s body: %w", method, path, err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.creds.BaseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.creds.Email, c.creds.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("jira %s %s: reading the response: %w", method, path, err)
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)])))
	}
	return data, nil
}
