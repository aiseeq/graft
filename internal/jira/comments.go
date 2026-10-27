package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// timeLayout is how Jira writes the time of a comment or a history entry.
const timeLayout = "2006-01-02T15:04:05.000-0700"

// Comment is one comment of an item.
type Comment struct {
	AuthorID string
	Created  time.Time
	// Text is the body with its formatting dropped: the text of each block
	// on a line of its own.
	Text string
}

// commentPage is how many comments one request reads.
const commentPage = 100

// Comments reads every comment of an item, oldest first.
func (c *Client) Comments(ctx context.Context, key string) ([]Comment, error) {
	var all []Comment
	for start := 0; ; {
		data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/rest/api/3/issue/%s/comment?startAt=%d&maxResults=%d&orderBy=created", key, start, commentPage), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page struct {
			Total    int `json:"total"`
			Comments []struct {
				Author struct {
					AccountID string `json:"accountId"`
				} `json:"author"`
				Created string          `json:"created"`
				Body    json.RawMessage `json:"body"`
			} `json:"comments"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("%s: unexpected comments JSON: %w", key, err)
		}
		for _, raw := range page.Comments {
			created, err := time.Parse(timeLayout, raw.Created)
			if err != nil {
				return nil, fmt.Errorf("%s: a comment created at %q: %w", key, raw.Created, err)
			}
			var body adfNode
			if err := json.Unmarshal(raw.Body, &body); err != nil {
				return nil, fmt.Errorf("%s: unexpected comment body: %w", key, err)
			}
			var text strings.Builder
			body.write(&text)
			all = append(all, Comment{AuthorID: raw.Author.AccountID, Created: created, Text: strings.TrimSpace(text.String())})
		}
		start += len(page.Comments)
		if start >= page.Total {
			return all, nil
		}
		if len(page.Comments) == 0 {
			return nil, fmt.Errorf("%s: comments page at %d is empty but %d remain", key, start, page.Total-start)
		}
	}
}

// adfNode is a node of the Atlassian document format a comment body is in.
type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
}

// write appends the text of the node: a block of text, a paragraph or a
// heading, ends with a line break, and inline nodes run on.
func (n *adfNode) write(b *strings.Builder) {
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
	case "hardBreak":
		b.WriteString("\n")
	}
	for i := range n.Content {
		n.Content[i].write(b)
	}
	switch n.Type {
	case "paragraph", "heading", "codeBlock":
		b.WriteString("\n")
	}
}

// CameInto returns when the item last came into one of the statuses from a
// status outside them: when its work began, for the statuses of work. It is
// zero when the history shows no such move, for an item created in one of
// them, say.
func (c *Client) CameInto(ctx context.Context, key string, statuses []string) (time.Time, error) {
	var last time.Time
	for start := 0; ; {
		data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/rest/api/3/issue/%s/changelog?startAt=%d&maxResults=%d", key, start, changelogPage), nil, http.StatusOK)
		if err != nil {
			return time.Time{}, err
		}
		var page struct {
			IsLast bool `json:"isLast"`
			Values []struct {
				Created string `json:"created"`
				Items   []struct {
					FieldID    string `json:"fieldId"`
					FromString string `json:"fromString"`
					ToString   string `json:"toString"`
				} `json:"items"`
			} `json:"values"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return time.Time{}, fmt.Errorf("%s: unexpected changelog JSON: %w", key, err)
		}
		for _, v := range page.Values {
			for _, it := range v.Items {
				if it.FieldID != "status" || !slices.Contains(statuses, it.ToString) || slices.Contains(statuses, it.FromString) {
					continue
				}
				when, err := time.Parse(timeLayout, v.Created)
				if err != nil {
					return time.Time{}, fmt.Errorf("%s: a history entry created at %q: %w", key, v.Created, err)
				}
				if when.After(last) {
					last = when
				}
			}
		}
		if page.IsLast {
			return last, nil
		}
		if len(page.Values) == 0 {
			return time.Time{}, fmt.Errorf("%s: changelog page at %d is empty but not the last", key, start)
		}
		start += len(page.Values)
	}
}
