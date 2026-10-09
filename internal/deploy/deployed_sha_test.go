package deploy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/envs"
)

// fakeSudo puts a sudo on PATH that opens locked for the command it runs, the
// way root sees a directory the deploy user cannot enter.
func fakeSudo(t *testing.T, locked, body string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LOCKED", locked)
}

const openingSudo = `[ "$1" = -n ] && shift
chmod 700 "$LOCKED"; "$@"; rc=$?; chmod 000 "$LOCKED"; exit $rc
`

func lockedSHA(t *testing.T, write bool) (*envs.Env, *config.DeployedSHA) {
	t.Helper()
	root := t.TempDir()
	locked := filepath.Join(root, "opt")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locked, "DEPLOYED_SHA")
	if write {
		if err := os.WriteFile(path, []byte("abc123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	return &envs.Env{Name: "test", Root: root}, &config.DeployedSHA{Path: path, Sudo: true}
}

func TestReadDeployedSHAChecksTheFileWithSudo(t *testing.T) {
	env, rec := lockedSHA(t, true)
	fakeSudo(t, filepath.Dir(rec.Path), openingSudo)
	for name, read := range map[string]func(context.Context, *envs.Env, *config.DeployedSHA) (string, error){
		"read": ReadDeployedSHA, "peek": PeekDeployedSHA,
	} {
		got, err := read(context.Background(), env, rec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != "abc123" {
			t.Fatalf("%s: got %q, want abc123", name, got)
		}
	}
}

func TestReadDeployedSHAReturnsEmptyWhenNothingWasRecorded(t *testing.T) {
	env, rec := lockedSHA(t, false)
	fakeSudo(t, filepath.Dir(rec.Path), openingSudo)
	got, err := ReadDeployedSHA(context.Background(), env, rec)
	if err != nil {
		t.Fatalf("ReadDeployedSHA: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want nothing recorded", got)
	}
}

func TestPeekDeployedSHAFailsWhenSudoRefuses(t *testing.T) {
	env, rec := lockedSHA(t, true)
	fakeSudo(t, filepath.Dir(rec.Path), "echo 'sudo: a password is required' >&2; exit 1\n")
	if got, err := PeekDeployedSHA(context.Background(), env, rec); err == nil {
		t.Fatalf("got %q and no error, want sudo's refusal", got)
	}
}
