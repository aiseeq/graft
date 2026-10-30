package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/jira"
)

func TestFreezeJournalFailureKeepsFreeze(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.mustGraft("deploy", "acc")
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	hook := filepath.Join(d.remote("origin"), "hooks", "update")
	writeFile(t, hook, "#!/bin/sh\ncase \"$1\" in refs/tags/freeze-log/*) exit 1;; esac\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := d.graft("", "deploy", "prod")
	if code == 0 {
		t.Fatalf("deploy with a failed journal push passed:\n%s", out)
	}
	if tags := d.remoteTags("freeze/prod/"); len(tags) != 1 {
		t.Errorf("freeze tags after journal failure = %q", tags)
	}
	if !strings.Contains(out, "freeze stays") || !strings.Contains(out, "journal") {
		t.Errorf("deploy did not explain that the incomplete journal keeps the freeze:\n%s", out)
	}
}

func TestFreezeHistoryUsesVersionIDWhenAnotherVersionExists(t *testing.T) {
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"isLast": true, "values": []any{
			map[string]any{"created": "2026-10-20T12:00:00.000+0000", "items": []any{
				map[string]any{"fieldId": "fixVersions", "from": "10001", "to": "10010", "fromString": "Product v1.2.9", "toString": "Product v1.2.9, Product v1.2.10"},
			}},
		}})
	}))
	defer srv.Close()
	got, has, err := jira.New(jira.Credentials{BaseURL: srv.URL}).FixVersionHistoryAfter(context.Background(), "PROJ-1", "10010", at)
	if err != nil || !got || !has {
		t.Errorf("FixVersionHistoryAfter = %v, %v, %v; want true, true, nil", got, has, err)
	}
}

func TestFreezeNoFreezeGateErrorHasNoPrefix(t *testing.T) {
	config := func(state string, q func(string) string) string {
		return strings.Replace(freezeConfig(state, q), "      freeze: {types: [Bug], advisory: [lint]}\n", "", 1)
	}
	d := newDeployFixture(t, config)
	d.write("change.txt", "change\n")
	writeFile(t, filepath.Join(d.state, "lint-fails"), "")
	out, code := d.graft("", "commit", "-m", "feat: PROJ-1 failing gate")
	if code == 0 {
		t.Fatalf("commit with a failing gate passed:\n%s", out)
	}
	if strings.Contains(out, "running commit gate:") {
		t.Errorf("gate error gained a prefix without a freeze:\n%s", out)
	}
}

func TestFreezePartialTagPushCleansAcceptedRemote(t *testing.T) {
	d := freezeRepo(t)
	mirror := d.remote("mirror")
	d.run(d.base, "git", "init", "-q", "--bare", mirror)
	d.addRemote("mirror", mirror)
	d.git("push", "-q", "mirror", "HEAD")
	hook := filepath.Join(mirror, "hooks", "update")
	writeFile(t, hook, "#!/bin/sh\ncase \"$1\" in refs/tags/freeze/*) exit 1;; esac\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	d.at("2026-10-20 10:00")
	out, code := d.graft("", "deploy", "freeze", "prod", "Product v1.2.10")
	if code == 0 {
		t.Fatalf("partial freeze push passed:\n%s", out)
	}
	if tags := d.remoteTags("freeze/prod/"); len(tags) != 0 {
		t.Errorf("origin still has freeze tags after partial push: %q", tags)
	}
	if tags := d.git("ls-remote", "--tags", "mirror", "refs/tags/freeze/prod/*"); tags != "" {
		t.Errorf("mirror still has freeze tags after partial push: %s", tags)
	}
	if tags := d.git("tag", "--list", "freeze/*"); tags != "" {
		t.Errorf("local freeze tags after partial push: %s", tags)
	}
	if strings.Contains(out, "retry with") || !strings.Contains(strings.ToLower(out), "freeze is not set") {
		t.Errorf("partial push error should say no freeze was set and have no retry hint:\n%s", out)
	}
	if branch := d.git("branch", "--show-current"); branch == "release/1.2.10" {
		t.Errorf("partial freeze push left the checkout on %s", branch)
	}
}

func TestFreezeCreatedTimeIsRequired(t *testing.T) {
	d := freezeRepo(t)
	d.commitFile("one.txt", "feat: PROJ-1 accepted work")
	d.freeze()
	d.jira.mu.Lock()
	delete(d.jira.created, "PROJ-10")
	d.jira.mu.Unlock()
	d.commitFile("new.txt", "feat: PROJ-10 created in frozen version")
	d.git("push", "-q", "origin", "HEAD")
	out, code := d.graft("", "deploy", "acc")
	if code == 0 || !strings.Contains(strings.ToLower(out), "created") {
		t.Errorf("missing created time was not an error: exit %d\n%s", code, out)
	}
}
