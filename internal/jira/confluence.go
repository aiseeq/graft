package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Page is a Confluence page with its storage-format body.
type Page struct {
	ID      string
	Title   string
	Version int
	Body    string
	// URL is the page's address in the browser.
	URL string
}

type rawPage struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	SpaceID string `json:"spaceId"`
	Version struct {
		Number int `json:"number"`
	} `json:"version"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
	} `json:"_links"`
}

func (c *Client) page(raw rawPage) Page {
	return Page{ID: raw.ID, Title: raw.Title, Version: raw.Version.Number, Body: raw.Body.Storage.Value, URL: c.creds.BaseURL + "/wiki" + raw.Links.WebUI}
}

// PageSpace returns the id of the space a page is in.
func (c *Client) PageSpace(ctx context.Context, id string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/wiki/api/v2/pages/"+url.PathEscape(id), nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var raw rawPage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("page %s: unexpected JSON: %w", id, err)
	}
	if raw.SpaceID == "" {
		return "", fmt.Errorf("page %s has no space", id)
	}
	return raw.SpaceID, nil
}

// FindPage returns the page of the title in the space, with its body; ok is
// false when there is none. A title is unique in a space.
func (c *Client) FindPage(ctx context.Context, spaceID, title string) (Page, bool, error) {
	q := url.Values{"space-id": {spaceID}, "title": {title}, "body-format": {"storage"}}
	data, err := c.do(ctx, http.MethodGet, "/wiki/api/v2/pages?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return Page{}, false, err
	}
	var found struct {
		Results []rawPage `json:"results"`
	}
	if err := json.Unmarshal(data, &found); err != nil {
		return Page{}, false, fmt.Errorf("pages titled %q: unexpected JSON: %w", title, err)
	}
	switch len(found.Results) {
	case 0:
		return Page{}, false, nil
	case 1:
		return c.page(found.Results[0]), true, nil
	default:
		return Page{}, false, fmt.Errorf("%d pages titled %q in space %s", len(found.Results), title, spaceID)
	}
}

// CreatePage adds a page under the parent.
func (c *Client) CreatePage(ctx context.Context, spaceID, parentID, title, body string) (Page, error) {
	req := map[string]any{
		"spaceId": spaceID, "parentId": parentID, "status": "current", "title": title,
		"body": map[string]string{"representation": "storage", "value": body},
	}
	data, err := c.do(ctx, http.MethodPost, "/wiki/api/v2/pages", req, http.StatusOK)
	if err != nil {
		return Page{}, err
	}
	var raw rawPage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Page{}, fmt.Errorf("page %q: unexpected JSON: %w", title, err)
	}
	return c.page(raw), nil
}

// UpdatePage replaces the page's body as its next version.
func (c *Client) UpdatePage(ctx context.Context, p Page, body, message string) (Page, error) {
	req := map[string]any{
		"id": p.ID, "status": "current", "title": p.Title,
		"body":    map[string]string{"representation": "storage", "value": body},
		"version": map[string]any{"number": p.Version + 1, "message": message},
	}
	data, err := c.do(ctx, http.MethodPut, "/wiki/api/v2/pages/"+url.PathEscape(p.ID), req, http.StatusOK)
	if err != nil {
		return Page{}, err
	}
	var raw rawPage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Page{}, fmt.Errorf("page %q: unexpected JSON: %w", p.Title, err)
	}
	return c.page(raw), nil
}

// IssueURL is the item's address in the browser.
func (c *Client) IssueURL(key string) string {
	return c.creds.BaseURL + "/browse/" + key
}

// VersionURL is the version's address in the browser.
func (c *Client) VersionURL(project, id string) string {
	return c.creds.BaseURL + "/projects/" + project + "/versions/" + id
}
