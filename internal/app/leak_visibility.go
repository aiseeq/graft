package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

var runGitHubVisibility = func(ctx context.Context, owner, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+owner+"/"+repo, "--jq", ".visibility")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var visibilityCache = struct {
	sync.Mutex
	values map[string]string
}{values: map[string]string{}}

func githubRepo(urlText string) (string, string, bool, error) {
	text := strings.TrimSpace(urlText)
	var path string
	if strings.Contains(text, "://") {
		u, err := url.Parse(text)
		if err != nil {
			return "", "", false, fmt.Errorf("parsing repository URL: %w", err)
		}
		if !strings.EqualFold(u.Hostname(), "github.com") {
			return "", "", false, nil
		}
		path = u.Path
	} else {
		if at := strings.LastIndex(text, "@"); at >= 0 {
			text = text[at+1:]
		}
		host, p, ok := strings.Cut(text, ":")
		if !ok || !strings.EqualFold(host, "github.com") {
			return "", "", false, nil
		}
		path = p
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false, nil
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	if parts[1] == "" {
		return "", "", false, nil
	}
	return parts[0], parts[1], true, nil
}

func remoteIsPublic(ctx context.Context, url string, l *userconfig.Leaks, log io.Writer) (bool, error) {
	if !l.RemoteIsPublic(url) {
		return false, nil
	}
	if !l.GitHubVisibility {
		return true, nil
	}
	owner, name, github, err := githubRepo(url)
	if err != nil {
		return false, err
	}
	if !github {
		return true, nil
	}
	key := owner + "/" + name
	visibilityCache.Lock()
	visibility, ok := visibilityCache.values[key]
	visibilityCache.Unlock()
	if !ok {
		var err error
		visibility, err = runGitHubVisibility(ctx, owner, name)
		if err != nil {
			fmt.Fprintf(log, "graft: cannot check GitHub visibility for %s: %v; treating it as public, add it to leaks.private_remotes if private or use git push --no-verify knowingly\n", key, err)
			visibility = "public"
		} else if visibility != "public" && visibility != "private" && visibility != "internal" {
			fmt.Fprintf(log, "graft: gh returned unexpected GitHub visibility %q for %s; treating it as public, add it to leaks.private_remotes if private or use git push --no-verify knowingly\n", visibility, key)
			visibility = "public"
		} else {
			visibilityCache.Lock()
			visibilityCache.values[key] = visibility
			visibilityCache.Unlock()
		}
	}
	return visibility == "public", nil
}
