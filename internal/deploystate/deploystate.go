package deploystate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

const (
	Env       = "GRAFT_DEPLOY_ACTIVE"
	pattern   = "deploy.*.reserve"
	directory = "deploys"
)

type Deployment struct {
	Project string `json:"project"`
	Target  string `json:"target"`
	// Started orders deployments: an install inside one waits only for those
	// that started before it, so two deploys installing at once do not wait
	// for each other. A marker without it (an older graft's) is waited for.
	Started time.Time `json:"started"`
}

func CacheDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("getting user cache directory: %w", err)
	}
	return filepath.Join(cache, "graft", directory), nil
}

// Register records a machine-wide deployment. Reserve's OS lock drops the
// record automatically if the process exits, including a forced kill.
func Register(project, target string) (func() error, error) {
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(Deployment{Project: project, Target: target, Started: time.Now()})
	if err != nil {
		return nil, fmt.Errorf("encoding deployment marker: %w", err)
	}
	release, err := lock.Reserve(dir, pattern, data)
	if err != nil {
		return nil, fmt.Errorf("registering deployment: %w", err)
	}
	return release, nil
}

func Active() ([]Deployment, error) {
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	files, err := lock.LiveData(dir, pattern)
	if err != nil {
		return nil, err
	}
	rows := make([]Deployment, 0, len(files))
	for _, file := range files {
		var row Deployment
		if err := json.Unmarshal(file.Data, &row); err != nil {
			return nil, fmt.Errorf("decoding deployment %s: %w", file.Path, err)
		}
		if row.Project == "" || row.Target == "" {
			return nil, fmt.Errorf("decoding deployment %s: project and target are required", file.Path)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func Identity(project, target string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(project + "\x00" + target))
}

func ChildIdentity(parent, project, target string) string {
	identity := Identity(project, target)
	if parent == "" {
		return identity
	}
	return parent + "," + identity
}

func OwnIdentity(identity string) bool {
	return slices.Contains(strings.Split(os.Getenv(Env), ","), identity)
}

// othersBefore are the deployments of rows an install waits for: outside a
// deploy all of them; inside one, not its own chain (named in the inherited
// environment) and only those that started before the earliest of the chain,
// so of two deploys installing at the same time the later one waits and the
// earlier one goes on.
func othersBefore(rows []Deployment) []Deployment {
	var own *Deployment
	for i := range rows {
		if OwnIdentity(Identity(rows[i].Project, rows[i].Target)) && (own == nil || earlier(rows[i], *own)) {
			own = &rows[i]
		}
	}
	var others []Deployment
	for _, row := range rows {
		// The chain's own markers start no earlier than its earliest.
		if own != nil && !earlier(row, *own) {
			continue
		}
		others = append(others, row)
	}
	return others
}

// earlier orders deployments by start, then by identity for a tie.
func earlier(a, b Deployment) bool {
	if !a.Started.Equal(b.Started) {
		return a.Started.Before(b.Started)
	}
	return Identity(a.Project, a.Target) < Identity(b.Project, b.Target)
}

// WaitForOthers waits until no deployment othersBefore names is active. A
// deploy that starts after the final scan can overlap the atomic local
// rename: a step it runs between the check and the rename sees the old
// binary, a later one the new one, never a half-written file.
func WaitForOthers(ctx context.Context, timeout time.Duration, stderr io.Writer) error {
	deadline := time.Now().Add(timeout)
	var shown string
	for {
		rows, err := Active()
		if err != nil {
			return fmt.Errorf("checking active deployments: %w", err)
		}
		others := othersBefore(rows)
		if len(others) == 0 {
			return nil
		}
		sort.Slice(others, func(i, j int) bool {
			if others[i].Project != others[j].Project {
				return others[i].Project < others[j].Project
			}
			return others[i].Target < others[j].Target
		})
		var lines []string
		for _, row := range others {
			lines = append(lines, fmt.Sprintf("project %s target %s", row.Project, row.Target))
		}
		names := strings.Join(lines, "; ")
		if names != shown {
			fmt.Fprintf(stderr, "graft: install: waiting for deploy: %s\n", names)
			shown = names
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("install: timed out waiting for deploy: %s", names)
		}
		pause := min(remaining, 100*time.Millisecond)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
