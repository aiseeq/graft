package app

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploystate"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/lock"
)

// TestBeginMachineDeploymentChainsTheIdentity: a deploy started inside
// another names both in the environment its steps inherit, so an install
// under the inner one skips the outer one's marker too; restore puts the
// outer chain back, or clears it when there was none.
func TestBeginMachineDeploymentChainsTheIdentity(t *testing.T) {
	for _, outer := range []string{deploystate.Identity("/src/projectA", "test"), ""} {
		t.Run("outer "+outer, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv(deploystate.Env, outer)
			lk, err := lock.Acquire(context.Background(), filepath.Join(t.TempDir(), "deploy.lock"), time.Second, "test", os.Stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lk.Release() }()
			release, restore, err := beginMachineDeployment("/src/projectB", "test", lk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			want := deploystate.ChildIdentity(outer, "/src/projectB", "test")
			if got := os.Getenv(deploystate.Env); got != want {
				t.Errorf("%s = %q, want %q", deploystate.Env, got, want)
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			got, set := os.LookupEnv(deploystate.Env)
			if got != outer || (outer == "" && set) {
				t.Errorf("after restore %s = %q (set %t), want %q", deploystate.Env, got, set, outer)
			}
		})
	}
}

func TestShipsNothingModuleBumpAgainstShippedGraph(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	for _, tc := range []struct {
		name, mainImport        string
		want                    bool
		reason                  string
		changeGo, changeReplace bool
	}{
		{"test only", "", true, "modules no shipped binary imports", false, false},
		{"shipped main", "golang.org/x/mod/semver", false, "imported by shipped binary", false, false},
		{"go list failure", "example.com/missing/pkg", false, "go list failed", false, false},
		{"go directive", "", false, "changes directives other than require", true, false},
		{"replace directive", "", false, "changes directives other than require", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			freezePolishGit(t, root, "init", "-q")
			freezePolishGit(t, root, "config", "user.name", "Test")
			freezePolishGit(t, root, "config", "user.email", "test@example.invalid")
			write := func(path, content string) {
				t.Helper()
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			goVersion := "1.26.0"
			if tc.changeGo {
				goVersion = "1.25.0"
			}
			write("go.mod", "module example.com/projectA\n\ngo "+goVersion+"\n")
			write("VERSION", "1.0.0\n")
			if tc.mainImport == "" {
				write("cmd/app/main.go", "package main\nfunc main() {}\n")
				if tc.name == "test only" {
					write("cmd/app/main_test.go", "package main\nimport (\"testing\"; \"golang.org/x/mod/semver\")\nfunc TestDependencyIsTestOnly(t *testing.T) { _ = semver.IsValid(\"v1.0.0\") }\n")
				}
			} else {
				write("cmd/app/main.go", "package main\nimport _ "+strconv.Quote(tc.mainImport)+"\nfunc main() {}\n")
			}
			freezePolishGit(t, root, "add", ".")
			freezePolishGit(t, root, "commit", "-m", "base")
			changedMod := "module example.com/projectA\n\ngo 1.26.0\n\nrequire golang.org/x/mod v0.42.0\n"
			if tc.changeReplace {
				changedMod += "replace golang.org/x/mod => golang.org/x/mod v0.42.0\n"
			}
			write("go.mod", changedMod)
			write("VERSION", "1.0.1\n")
			write("docs/ignored.md", "service change\n")
			freezePolishGit(t, root, "add", ".")
			freezePolishGit(t, root, "commit", "-m", "module bump")
			repo, err := gitx.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			hooks, hooked := t.TempDir(), filepath.Join(t.TempDir(), "hooked")
			if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+strconv.Quote(hooked)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			freezePolishGit(t, root, "config", "core.hooksPath", hooks)
			defer func() {
				if _, err := os.Stat(hooked); err == nil {
					t.Error("the check ran the repository's post-checkout hook")
				}
				if list := freezePolishGit(t, root, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
					t.Errorf("worktrees left after the check:\n%s", list)
				}
			}()
			head := freezePolishGit(t, root, "rev-parse", "HEAD")
			conf := &config.ApprovedRelease{Ignore: []*regexp.Regexp{regexp.MustCompile(`^docs/`)}}
			cfg := &config.Config{Version: config.Version{Mode: config.ModeFile, File: "VERSION"}}
			ok, why, err := shipsNothing(repo, cfg, conf, head)
			if tc.reason == "go list failed" {
				if err == nil || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("shipsNothing error = %v, want %q", err, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.want || !strings.Contains(why, tc.reason) {
				t.Fatalf("shipsNothing = (%t, %q), want (%t, reason containing %q)", ok, why, tc.want, tc.reason)
			}
		})
	}
}
