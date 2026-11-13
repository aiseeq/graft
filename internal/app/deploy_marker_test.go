package app

import (
	"context"
	"os"
	"os/exec"
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

// moduleBumpRepo is a repository of example.com/projectA whose modules a and
// b come from local directories by replace: a v1.1.0 requires b v1.1.0.
func moduleBumpRepo(t *testing.T, mainFile, mainSrc, goVersion, require string) (string, func(string, string)) {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	freezePolishGit(t, root, "init", "-q")
	freezePolishGit(t, root, "config", "user.name", "Test")
	freezePolishGit(t, root, "config", "user.email", "test@example.invalid")
	for dir, mod := range map[string]string{
		"mods/a1": "module example.com/a\n\ngo 1.21\n",
		"mods/a2": "module example.com/a\n\ngo 1.21\n\nrequire example.com/b v1.1.0\n",
		"mods/b1": "module example.com/b\n\ngo 1.21\n",
		"mods/b2": "module example.com/b\n\ngo 1.21\n",
	} {
		write(dir+"/go.mod", mod)
		write(dir+"/x.go", "package "+mod[len("module example.com/"):len("module example.com/")+1]+"\n\nconst Dir = "+strconv.Quote(dir)+"\n")
	}
	write("go.mod", moduleBumpGoMod(goVersion, require, ""))
	write("VERSION", "1.0.0\n")
	write(mainFile, mainSrc)
	write("cmd/app/main_test.go", "package main\n\nimport (\n\t\"testing\"\n\n\t\"example.com/a\"\n)\n\nfunc TestA(t *testing.T) { _ = a.Dir }\n")
	freezePolishGit(t, root, "add", ".")
	freezePolishGit(t, root, "commit", "-q", "-m", "base")
	return root, write
}

func moduleBumpGoMod(goVersion, require, extra string) string {
	return "module example.com/projectA\n\ngo " + goVersion + "\n\nrequire (\n" + require + ")\n\nreplace (\n" +
		"\texample.com/a v1.0.0 => ./mods/a1\n\texample.com/a v1.1.0 => ./mods/a2\n" +
		"\texample.com/b v1.0.0 => ./mods/b1\n\texample.com/b v1.1.0 => ./mods/b2\n)\n" + extra
}

func TestShipsNothingModuleBumpAgainstShippedGraph(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	const (
		aOld   = "\texample.com/a v1.0.0\n"
		aNew   = "\texample.com/a v1.1.0\n"
		bOld   = "\texample.com/b v1.0.0\n"
		noMain = "package main\n\nfunc main() {}\n"
	)
	imports := func(pkg string) string {
		return "package main\n\nimport _ " + strconv.Quote(pkg) + "\n\nfunc main() {}\n"
	}
	for _, tc := range []struct {
		name, mainFile, mainSrc string
		before, after           string // requirements
		goAfter, extraAfter     string
		noIgnore, wantErr       bool
		want                    bool
		reason                  string
	}{
		{name: "a module only tests import", mainSrc: noMain, before: aOld, after: aNew, want: true, reason: "stay as they were (example.com/a)"},
		{name: "no ignore_paths", mainSrc: noMain, before: aOld, after: aNew, noIgnore: true, want: true, reason: "stay as they were"},
		{name: "a module the shipped main imports", mainSrc: imports("example.com/a"), before: aOld, after: aNew, reason: "changes modules of a shipped binary: example.com/a"},
		{name: "a module the shipped main gets through another", mainSrc: imports("example.com/b"), before: aOld + bOld, after: aNew + bOld, reason: "changes modules of a shipped binary: example.com/b"},
		{name: "a main under ignore_paths", mainFile: "docs/tool/main.go", mainSrc: imports("example.com/a"), before: aOld, after: aNew, want: true, reason: "stay as they were"},
		{name: "go list failure", mainSrc: imports("example.com/missing/pkg"), before: aOld, after: aNew, wantErr: true, reason: "go list failed"},
		{name: "go directive", mainSrc: noMain, before: aOld, after: aOld, goAfter: "1.22", reason: "changes directives other than require"},
		{name: "replace directive", mainSrc: noMain, before: aOld, after: aOld, extraAfter: "replace example.com/c => ./mods/b1\n", reason: "changes directives other than require"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mainFile := tc.mainFile
			if mainFile == "" {
				mainFile = "cmd/app/main.go"
			}
			if mainFile != "cmd/app/main.go" {
				tc.mainSrc, mainFile = noMain, "cmd/app/main.go"
			}
			root, write := moduleBumpRepo(t, mainFile, tc.mainSrc, "1.21", tc.before)
			if tc.mainFile != "" {
				write(tc.mainFile, imports("example.com/a"))
				freezePolishGit(t, root, "add", ".")
				freezePolishGit(t, root, "commit", "-q", "-m", "ignored tool")
			}
			goAfter := tc.goAfter
			if goAfter == "" {
				goAfter = "1.21"
			}
			write("go.mod", moduleBumpGoMod(goAfter, tc.after, tc.extraAfter))
			write("VERSION", "1.0.1\n")
			conf := &config.ApprovedRelease{}
			if !tc.noIgnore {
				write("docs/ignored.md", "service change\n")
				conf.Ignore = []*regexp.Regexp{regexp.MustCompile(`^docs/`)}
			}
			freezePolishGit(t, root, "add", ".")
			freezePolishGit(t, root, "commit", "-q", "-m", "module bump")
			hooks, hooked := t.TempDir(), filepath.Join(t.TempDir(), "hooked")
			if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+strconv.Quote(hooked)+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			freezePolishGit(t, root, "config", "core.hooksPath", hooks)
			repo, err := gitx.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Version: config.Version{Mode: config.ModeFile, File: "VERSION"}}
			ok, why, err := shipsNothing(repo, cfg, conf, freezePolishGit(t, root, "rev-parse", "HEAD"))
			if tc.wantErr {
				if err == nil || ok || !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("shipsNothing = (%t, %q, %v), want an error containing %q", ok, why, err, tc.reason)
				}
			} else if err != nil || ok != tc.want || !strings.Contains(why, tc.reason) {
				t.Errorf("shipsNothing = (%t, %q, %v), want (%t, reason containing %q)", ok, why, err, tc.want, tc.reason)
			}
			if _, err := os.Stat(hooked); err == nil {
				t.Error("the check ran the repository's post-checkout hook")
			}
			if list := freezePolishGit(t, root, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
				t.Errorf("worktrees left after the check:\n%s", list)
			}
			if status := freezePolishGit(t, root, "status", "--porcelain"); status != "" {
				t.Errorf("the check changed the repository's tree:\n%s", status)
			}
		})
	}
}

func TestShipsNothingNestedModuleBumpReachesTheRootBinary(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	main := "package main\n\nimport _ \"example.com/nested\"\n\nfunc main() {}\n"
	root, write := moduleBumpRepo(t, "cmd/app/main.go", main, "1.21", "\texample.com/nested v0.0.0\n")
	nested := func(b string) string {
		return "module example.com/nested\n\ngo 1.21\n\nrequire example.com/b " + b + "\n"
	}
	write("go.mod", moduleBumpGoMod("1.21", "\texample.com/nested v0.0.0\n", "replace example.com/nested => ./nested\n"))
	write("nested/go.mod", nested("v1.0.0"))
	write("nested/x.go", "package nested\n\nimport _ \"example.com/b\"\n")
	freezePolishGit(t, root, "add", ".")
	freezePolishGit(t, root, "commit", "-q", "-m", "nested")
	write("nested/go.mod", nested("v1.1.0"))
	freezePolishGit(t, root, "add", ".")
	freezePolishGit(t, root, "commit", "-q", "-m", "nested bump")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Version: config.Version{Mode: config.ModeFile, File: "VERSION"}}
	ok, why, err := shipsNothing(repo, cfg, &config.ApprovedRelease{}, freezePolishGit(t, root, "rev-parse", "HEAD"))
	if err != nil || ok || !strings.Contains(why, "example.com/b") {
		t.Errorf("shipsNothing = (%t, %q, %v), want the root binary's example.com/b named", ok, why, err)
	}
}

func TestShipsNothingVendoredModuleBump(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "")
	main := "package main\n\nimport _ \"example.com/b\"\n\nfunc main() {}\n"
	root, write := moduleBumpRepo(t, "cmd/app/main.go", main, "1.21", "\texample.com/a v1.0.0\n\texample.com/b v1.0.0\n")
	vendor := exec.Command("go", "mod", "vendor")
	vendor.Dir = root
	if out, err := vendor.CombinedOutput(); err != nil {
		t.Fatalf("go mod vendor: %v\n%s", err, out)
	}
	freezePolishGit(t, root, "add", ".")
	freezePolishGit(t, root, "commit", "-q", "-m", "vendor")
	write("go.sum", "example.com/z v1.0.0/go.mod h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
	freezePolishGit(t, root, "add", ".")
	freezePolishGit(t, root, "commit", "-q", "-m", "go.sum only")
	repo, err := gitx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Version: config.Version{Mode: config.ModeFile, File: "VERSION"}}
	ok, why, err := shipsNothing(repo, cfg, &config.ApprovedRelease{}, freezePolishGit(t, root, "rev-parse", "HEAD"))
	if err != nil || !ok {
		t.Errorf("vendored go.sum change, shipsNothing = (%t, %q, %v), want it to pass", ok, why, err)
	}
}
