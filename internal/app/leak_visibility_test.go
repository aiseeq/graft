package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func TestGitHubRepoURLs(t *testing.T) {
	for _, raw := range []string{
		"https://github.com/owner/projectA",
		"https://github.com/owner/projectA.git",
		"ssh://git@github.com/owner/projectA.git",
		"ssh://github.com/owner/projectA",
		"git@github.com:owner/projectA.git",
		"git@github.com:owner/projectA",
	} {
		owner, repo, ok, err := githubRepo(raw)
		if err != nil || !ok || owner != "owner" || repo != "projectA" {
			t.Errorf("githubRepo(%q) = %q, %q, %v, %v", raw, owner, repo, ok, err)
		}
	}
}

func TestGitHubVisibilityControlsLeakCheck(t *testing.T) {
	oldRun := runGitHubVisibility
	oldCache := visibilityCache.values
	t.Cleanup(func() { runGitHubVisibility = oldRun; visibilityCache.values = oldCache })
	visibilityCache.values = map[string]string{}
	calls := 0
	runGitHubVisibility = func(_ context.Context, owner, repo string) (string, error) {
		calls++
		if owner != "owner" || repo != "projectA" {
			t.Fatalf("unexpected repository %s/%s", owner, repo)
		}
		return "private", nil
	}
	l := &userconfig.Leaks{PublicRemotes: []string{"github.com/owner/"}, GitHubVisibility: true}
	var stderr bytes.Buffer
	for range 2 {
		public, err := remoteIsPublic(context.Background(), "git@github.com:owner/projectA.git", l, &stderr)
		if err != nil || public {
			t.Fatalf("private visibility: public=%v err=%v", public, err)
		}
	}
	if calls != 1 {
		t.Fatalf("visibility queried %d times, want cached once", calls)
	}
	runGitHubVisibility = func(_ context.Context, _, _ string) (string, error) {
		calls++
		return "public", nil
	}
	visibilityCache.values = map[string]string{}
	public, err := remoteIsPublic(context.Background(), "https://github.com/owner/projectA", l, &stderr)
	if err != nil || !public {
		t.Fatalf("public visibility: public=%v err=%v", public, err)
	}
	private := *l
	private.PrivateRemotes = []string{"github.com/owner/projectA"}
	if public, err := remoteIsPublic(context.Background(), "https://github.com/owner/projectA", &private, &stderr); err != nil || public {
		t.Fatalf("private prefix: public=%v err=%v", public, err)
	}
	runGitHubVisibility = func(context.Context, string, string) (string, error) { return "", errors.New("gh unavailable") }
	visibilityCache.values = map[string]string{}
	public, err = remoteIsPublic(context.Background(), "https://github.com/owner/projectA", l, &stderr)
	if err != nil || !public || !strings.Contains(stderr.String(), "gh unavailable") || !strings.Contains(stderr.String(), "private_remotes") {
		t.Fatalf("failed lookup: public=%v err=%v stderr=%q", public, err, stderr.String())
	}
	calls = 0
	if public, err := remoteIsPublic(context.Background(), "https://code.example/owner/projectA", l, &stderr); err != nil || public || calls != 0 {
		t.Fatalf("non-GitHub URL: public=%v err=%v calls=%d", public, err, calls)
	}
}

func TestGitHubVisibilityUnexpectedOutputFailsOpen(t *testing.T) {
	oldRun := runGitHubVisibility
	oldCache := visibilityCache.values
	t.Cleanup(func() { runGitHubVisibility = oldRun; visibilityCache.values = oldCache })
	l := &userconfig.Leaks{PublicRemotes: []string{"github.com/owner/"}, GitHubVisibility: true}
	for _, value := range []string{"", "unknown"} {
		t.Run(value, func(t *testing.T) {
			visibilityCache.values = map[string]string{}
			runGitHubVisibility = func(context.Context, string, string) (string, error) { return value, nil }
			var stderr bytes.Buffer
			public, err := remoteIsPublic(context.Background(), "https://github.com/owner/projectA", l, &stderr)
			if err != nil || !public || !strings.Contains(stderr.String(), fmt.Sprintf("%q", value)) ||
				!strings.Contains(stderr.String(), "treating it as public") || !strings.Contains(stderr.String(), "private_remotes") {
				t.Fatalf("unexpected visibility %q: public=%v err=%v stderr=%q", value, public, err, stderr.String())
			}
		})
	}
}
