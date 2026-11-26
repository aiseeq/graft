package tasks

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

// A gate that did not split runs its steps with remote_heavy off, so
// r.remoteHeavy is nil while the recorded price still asks whether a
// remote: true task's glint step could go to the host. The answer comes
// from the settings the gate decided by, not from the switched-off field.
func TestRecordedGateCostReasonGlintStepWithRemoteHeavyOff(t *testing.T) {
	remote := true
	r := &Runner{
		Config: &config.Config{Tasks: map[string]*config.Task{
			"lint": {Remote: &remote, Run: []config.Step{{Source: "glint check ./...", Argv: []string{"glint", "check", "./..."}}}},
		}},
		Stderr:     io.Discard,
		noRepoName: true,
		gateCosts: &gateStepCosts{
			local:  map[string]gateStepCost{},
			host:   "pc",
			hasFee: true,
			fee:    40,
			remote: &config.RemoteHeavy{Host: "pc"},
		},
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if got := r.recordedGateCostReason(&frame{task: "lint"}, "glint check ./...", false); got != "" {
		t.Fatalf("reason = %q, want none: a glint missing here is not eligible, so no price reason applies", got)
	}
	if r.remoteHeavy != nil {
		t.Fatalf("remoteHeavy left as %+v, want it restored to nil", r.remoteHeavy)
	}
	got := r.glintStayReason("glint")
	if want := "remote_heavy is off"; got != want {
		t.Fatalf("stay reason with remote_heavy off = %q, want %q", got, want)
	}

	// The glint here is the copy install recorded on the host, so the step
	// is eligible by the gate's settings and its missing price is the reason.
	bin := t.TempDir()
	glint := filepath.Join(bin, "glint")
	body := []byte("#!/bin/sh\n")
	if err := os.WriteFile(glint, body, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("XDG_CACHE_HOME", cache)
	sum := sha256.Sum256(body)
	hosts, err := json.Marshal(map[string]map[string]string{"pc": {glint: fmt.Sprintf("%x", sum)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cache, "graft"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "graft", "hosts.json"), hosts, 0o600); err != nil {
		t.Fatal(err)
	}
	r.glintCopies = nil
	want := "price probe here: no fresh local price, 40.0s on pc"
	if got := r.recordedGateCostReason(&frame{task: "lint"}, "glint check ./...", false); got != want {
		t.Fatalf("reason for an eligible glint step = %q, want %q", got, want)
	}
	if r.remoteHeavy != nil {
		t.Fatalf("remoteHeavy left as %+v, want it restored to nil", r.remoteHeavy)
	}
}
