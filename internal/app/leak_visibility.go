package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// githubRepo is the owner and name of a GitHub repository URL; ok is false
// for another host or a path that is not owner/name.
func githubRepo(urlText string) (owner, name string, ok bool) {
	rest, found := strings.CutPrefix(userconfig.NormalizeRemote(strings.TrimSpace(urlText)), "github.com/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" {
		return "", "", false
	}
	name = strings.TrimSuffix(parts[1], ".git")
	if name == "" {
		return "", "", false
	}
	return parts[0], name, true
}

func remoteIsPublic(ctx context.Context, url string, l *userconfig.Leaks, log io.Writer) (bool, error) {
	if !l.RemoteIsPublic(url) {
		return false, nil
	}
	if !l.GitHubVisibility {
		return true, nil
	}
	owner, name, github := githubRepo(url)
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
