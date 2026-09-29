package jira

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.env")
	if err := os.WriteFile(good, []byte("JIRA_BASE_URL=https://example.atlassian.net/\nJIRA_EMAIL=me@example.com\nJIRA_API_TOKEN='tok'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCredentials(good)
	if err != nil || c.BaseURL != "https://example.atlassian.net" || c.Token != "tok" {
		t.Errorf("%+v %v", c, err)
	}
	partial := filepath.Join(dir, "partial.env")
	if err := os.WriteFile(partial, []byte("JIRA_BASE_URL=https://x\nJIRA_API_TOKEN=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(partial); err == nil || !strings.Contains(err.Error(), "JIRA_API_TOKEN, JIRA_EMAIL") {
		t.Errorf("missing keys: %v", err)
	}
}
