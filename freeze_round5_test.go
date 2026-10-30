package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/lock"
)

func TestDeployMissingKeyPreflightRunsBeforeLockAndRequests(t *testing.T) {
	d := newDeployFixture(t, func(state string, q func(string) string) string {
		cfg := deployConfig(state, q)
		cfg = strings.Replace(cfg, "deploy:\n", "tasks:\n  needs-key:\n    run: [[sh, -c, 'echo ${NEED_KEY}']]\ndeploy:\n", 1)
		cfg = strings.Replace(cfg, "    test:\n      env: local\n", "    test:\n      env: local\n      deps: [needs-key]\n", 1)
		return cfg + "\nlock:\n  timeout: 100ms\n"
	})
	lockPath := filepath.Join(d.work, ".git", "graft-locks", "deploy-test.lock")
	held, err := lock.AcquireMode(context.Background(), lockPath, lock.Exclusive, 0, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := held.Release(); err != nil {
			t.Errorf("release deploy lock: %v", err)
		}
	}()

	out, code := d.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "deploy test: nothing was done, missing:") || !strings.Contains(out, "NEED_KEY is not set") {
		t.Fatalf("missing-key preflight: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "still held after") {
		t.Errorf("deploy lock was taken before preflight:\n%s", out)
	}
	d.jira.mu.Lock()
	requests := d.jira.requests
	d.jira.mu.Unlock()
	if requests != 0 {
		t.Errorf("preflight made %d Jira requests, want none", requests)
	}
	if strings.Contains(out, "test-sha") || strings.Contains(out, "deployed SHA") {
		t.Errorf("preflight read deployed_sha before reporting the missing key:\n%s", out)
	}
}
