package app

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestWorktreeSyncGoModHintRequiresOnlyRequireVersionChange(t *testing.T) {
	ours := "module example.com/projectA\n\ngo 1.25\n\nrequire example.com/akit v1.9.0\n\nreplace example.com/dep => ../worktree\n"
	theirs := "module example.com/projectA\n\ngo 1.27\n\nrequire example.com/akit v1.10.0\n\nreplace example.com/dep => ../trunk\n"
	hint, err := worktreeSyncGoModVersionHint(ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if hint != "" {
		t.Fatalf("other go.mod directives changed, but got a version hint: %s", hint)
	}
}

func TestWorktreeSyncLatestGoModThreeWay(t *testing.T) {
	base := "module example.com/projectA\n\ngo 1.23\n\nrequire example.com/lib v1.0.0\n"
	onto := strings.Replace(base, "v1.0.0", "v1.1.0", 1)
	commit := strings.Replace(base, "v1.0.0", "v1.2.0", 1)
	changes, ok, err := worktreeSyncLatestGoModChanges(base, onto, commit, func(path string) bool { return path == "example.com/lib" })
	if err != nil || !ok || len(changes) != 1 || changes[0].version != "v1.2.0" {
		t.Fatalf("three-way changes = %#v, %v, %v", changes, ok, err)
	}
}

func TestWorktreeSyncLatestGoModRejectsUnsafeChanges(t *testing.T) {
	base := "module example.com/projectA\n\ngo 1.23\n\nrequire example.com/lib v1.1.0\n"
	tests := []struct{ name, onto, commit string }{
		{"downgrade", strings.Replace(base, "v1.1.0", "v1.2.0", 1), strings.Replace(base, "v1.1.0", "v1.0.0", 1)},
		{"other require", strings.Replace(base, "v1.1.0", "v1.2.0", 1), strings.Replace(base, "v1.1.0", "v1.3.0", 1) + "\nrequire example.com/other v1.0.0\n"},
		{"replace", strings.Replace(base, "v1.1.0", "v1.2.0", 1) + "\nreplace example.com/old => example.com/new v1.0.0\n", strings.Replace(base, "v1.1.0", "v1.3.0", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := worktreeSyncLatestGoModChanges(base, tt.onto, tt.commit, func(path string) bool { return path == "example.com/lib" })
			if err != nil || ok {
				t.Fatalf("unsafe change accepted: ok=%v err=%v", ok, err)
			}
		})
	}
}

func TestWorktreeSyncRejectsConflictOutsideGoModule(t *testing.T) {
	f := newSyncPushFixture(t, false)
	f.git(f.work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(f.work, "base"), []byte("feature edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "base")
	f.git(f.work, "commit", "-q", "-m", "feature edit")
	f.git(f.work, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(f.work, "base"), []byte("main edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(f.work, "add", "base")
	f.git(f.work, "commit", "-q", "-m", "main edit")
	f.git(f.work, "checkout", "-q", "feature")
	err := (&App{Dir: f.work, Stdout: io.Discard, Stderr: io.Discard}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "main"})
	if err == nil || !strings.Contains(err.Error(), "a commit of the branch conflicts with main: resolve and git rebase --continue") {
		t.Fatalf("sync conflict error = %v", err)
	}
	if !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.work)) {
		t.Fatal("rebase did not remain stopped")
	}
}

func mustOpenTestRepo(t *testing.T, dir string) *gitx.Repo {
	t.Helper()
	repo, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestWorktreeSyncResolvesLatestModuleConflictOffline(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"lib": "v1.2.0"}, true)
	f.commit("feature raise")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"lib": "v1.1.0"}, true)
	f.commit("main raise")
	f.git("checkout", "-q", "feature")
	var stderr bytes.Buffer
	if err := f.sync(&stderr); err != nil {
		t.Fatalf("%v; stderr=%s", err, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(f.repo, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "example.com/lib v1.2.0") {
		t.Fatalf("go.mod = %s", data)
	}
	if !strings.Contains(stderr.String(), "example.com/lib (v1.1.0 vs v1.2.0), took v1.2.0") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	cmd := exec.Command("go", "mod", "verify")
	cmd.Dir = f.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod verify: %v: %s", err, out)
	}
}

func TestWorktreeSyncRejectsRollbackAfterEarlierCommit(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"liba", "libm", "libz"})
	f.setVersions(map[string]string{"liba": "v1.0.0", "libm": "v1.0.0", "libz": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"liba": "v1.5.0", "libm": "v1.0.0", "libz": "v1.0.0"}, true)
	f.commit("C1 raise liba")
	f.setVersions(map[string]string{"liba": "v1.2.0", "libm": "v1.0.0", "libz": "v1.2.0"}, true)
	f.commit("C2 rollback liba, raise libz")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"liba": "v1.0.0", "libm": "v1.0.0", "libz": "v1.1.0"}, true)
	f.commit("main raise libz")
	f.git("checkout", "-q", "feature")
	err := f.sync(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a commit of the branch conflicts with main") {
		t.Fatalf("sync error = %v", err)
	}
	if !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.repo)) {
		t.Fatal("rebase should stop at C2 downgrade")
	}
	onto := f.git("show", "HEAD:go.mod")
	if !strings.Contains(onto, "example.com/liba v1.5.0") {
		t.Fatalf("C1 version was not preserved on onto: %s", onto)
	}
}

func TestWorktreeSyncPrintsOneLinePerResolvedCommit(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"liba", "libm", "libz"})
	f.setVersions(map[string]string{"liba": "v1.0.0", "libm": "v1.0.0", "libz": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"liba": "v1.2.0", "libm": "v1.0.0", "libz": "v1.0.0"}, true)
	f.commit("C1 raise liba")
	f.setVersions(map[string]string{"liba": "v1.2.0", "libm": "v1.0.0", "libz": "v1.5.0"}, true)
	f.commit("C2 raise libz")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"liba": "v1.1.0", "libm": "v1.0.0", "libz": "v1.1.0"}, true)
	f.commit("main raises")
	f.git("checkout", "-q", "feature")
	var stderr bytes.Buffer
	if err := f.sync(&stderr); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "example.com/liba") || strings.Contains(lines[0], "libz") || !strings.Contains(lines[1], "example.com/libz") || strings.Contains(lines[1], "liba") {
		t.Fatalf("resolution stderr = %q", stderr.String())
	}
}

func TestWorktreeSyncRejectsExtraGoModLine(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"lib": "v1.2.0"}, true)
	if err := os.WriteFile(filepath.Join(f.repo, "go.mod"), []byte(strings.TrimSpace(string(f.read("go.mod")))+"\n\nreplace example.com/old => example.com/new v1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.commit("feature raise with replace")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"lib": "v1.1.0"}, true)
	f.commit("main raise")
	f.git("checkout", "-q", "feature")
	err := f.sync(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a commit of the branch conflicts with main") || !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.repo)) {
		t.Fatalf("sync error = %v", err)
	}
}

func TestWorktreeSyncRejectsTidyChangesOutsideRaisedRequires(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	writeTestModuleProxy(t, filepath.Join(f.root, "proxy"), []string{"unused"})
	addUnusedRequire := func() {
		t.Helper()
		data := append(f.read("go.mod"), []byte("\nrequire example.com/unused v1.0.0\n")...)
		if err := os.WriteFile(filepath.Join(f.repo, "go.mod"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("go", "mod", "download", "example.com/unused@v1.0.0")
		cmd.Dir = f.repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("download unused module: %v: %s", err, out)
		}
	}
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	addUnusedRequire()
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"lib": "v1.2.0"}, true)
	addUnusedRequire()
	f.commit("feature raise")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"lib": "v1.1.0"}, true)
	addUnusedRequire()
	f.commit("main raise")
	f.git("checkout", "-q", "feature")
	err := f.sync(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "go mod tidy changed go.mod outside the raised require lines") {
		t.Fatalf("sync error = %v", err)
	}
	if !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.repo)) || !strings.Contains(string(f.read("go.mod")), "<<<<<<<") {
		t.Fatal("tidy refusal did not restore the stopped conflict")
	}
}

func TestWorktreeSyncRejectsVendoredModuleConflict(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"lib": "v1.2.0"}, true)
	f.commit("feature raise")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"lib": "v1.1.0"}, true)
	if err := os.MkdirAll(filepath.Join(f.repo, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "vendor", "modules.txt"), []byte("# vendor manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.commit("main raise with vendor")
	f.git("checkout", "-q", "feature")
	err := f.sync(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a commit of the branch conflicts with main") || !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.repo)) {
		t.Fatalf("sync error = %v", err)
	}
}

func TestWorktreeSyncRestoresConflictFilesWhenDownloadFails(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	versionDir := filepath.Join(f.root, "proxy", "example.com", "lib", "@v")
	for _, suffix := range []string{".mod", ".info", ".zip"} {
		if err := os.Remove(filepath.Join(versionDir, "v1.2.0"+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	f.commit("base")
	f.git("checkout", "-q", "-b", "feature")
	f.setVersions(map[string]string{"lib": "v1.2.0"}, false)
	f.commit("feature raise to missing version")
	f.git("checkout", "-q", "main")
	f.setVersions(map[string]string{"lib": "v1.1.0"}, true)
	f.commit("main raise")
	f.git("checkout", "-q", "feature")
	err := f.sync(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "go mod download example.com/lib@v1.2.0 failed") {
		t.Fatalf("sync error = %v", err)
	}
	if !worktreeSyncRebaseStopped(mustOpenTestRepo(t, f.repo)) {
		t.Fatal("rebase should remain stopped after download failure")
	}
	if !strings.Contains(string(f.read("go.mod")), "<<<<<<<") {
		t.Fatalf("go.mod conflict markers were not restored: %s", f.read("go.mod"))
	}
}

func TestWorktreeSyncResetGoSumAcceptsMissingOntoFile(t *testing.T) {
	f := newModuleSyncFixture(t, []string{"lib"})
	f.setVersions(map[string]string{"lib": "v1.0.0"}, true)
	f.commit("base with sum")
	f.git("rm", "-q", "go.sum")
	f.commit("remove sum")
	sumPath := filepath.Join(f.repo, "go.sum")
	if err := os.WriteFile(sumPath, []byte("stale sum\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := mustOpenTestRepo(t, f.repo)
	if err := worktreeSyncResetGoSumToOnto(repo, ".", sumPath); err != nil {
		t.Fatalf("reset go.sum absent from HEAD: %v", err)
	}
	if _, err := os.Stat(sumPath); !os.IsNotExist(err) {
		t.Errorf("go.sum should be removed, stat error = %v", err)
	}
}

type moduleSyncFixture struct {
	t       *testing.T
	root    string
	repo    string
	modules []string
}

func newModuleSyncFixture(t *testing.T, modules []string) *moduleSyncFixture {
	t.Helper()
	root := t.TempDir()
	proxy := filepath.Join(root, "proxy")
	writeTestModuleProxy(t, proxy, modules)
	configHome := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	var latest []string
	for _, module := range modules {
		latest = append(latest, "example.com/"+module)
	}
	settings := "modules:\n  latest: [" + strings.Join(latest, ", ") + "]\n"
	if err := os.WriteFile(filepath.Join(configHome, "graft", "config.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("GOPROXY", "file://"+proxy)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOPATH", filepath.Join(root, "gopath"))
	t.Setenv("GOMODCACHE", filepath.Join(root, "gomodcache"))
	t.Setenv("GOCACHE", filepath.Join(root, "gocache"))
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &moduleSyncFixture{t: t, root: root, repo: repo, modules: modules}
	f.git("init", "-q", "-b", "main")
	f.git("config", "user.name", "Test")
	f.git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, ".graft.yaml"), []byte("schema: 1\nversion: {mode: none}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *moduleSyncFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *moduleSyncFixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.repo, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *moduleSyncFixture) setVersions(versions map[string]string, download bool) {
	f.t.Helper()
	var requires, imports []string
	for _, module := range f.modules {
		version := versions[module]
		requires = append(requires, "\texample.com/"+module+" "+version)
		imports = append(imports, "_ \"example.com/"+module+"\"")
	}
	goMod := "module example.com/projectA\n\ngo 1.23.0\n\nrequire (\n" + strings.Join(requires, "\n") + "\n)\n"
	if err := os.WriteFile(filepath.Join(f.repo, "go.mod"), []byte(goMod), 0o600); err != nil {
		f.t.Fatal(err)
	}
	source := "package projectA\n\nimport (\n" + strings.Join(imports, "\n") + "\n)\n"
	if err := os.WriteFile(filepath.Join(f.repo, "project.go"), []byte(source), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if !download {
		return
	}
	for _, module := range f.modules {
		cmd := exec.Command("go", "mod", "download", "example.com/"+module+"@"+versions[module])
		cmd.Dir = f.repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			f.t.Fatalf("go mod download %s: %v: %s", module, err, out)
		}
	}
}

func (f *moduleSyncFixture) commit(message string) {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-q", "-m", message)
}

func (f *moduleSyncFixture) sync(stderr io.Writer) error {
	f.t.Helper()
	return (&App{Dir: f.repo, Stdout: io.Discard, Stderr: stderr}).WorktreeSync(context.Background(), WorktreeSyncOptions{Trunk: "main"})
}

func writeTestModuleProxy(t *testing.T, root string, modules []string) {
	t.Helper()
	for _, module := range modules {
		dir := filepath.Join(root, "example.com", module, "@v")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var versions []string
		for _, version := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.5.0"} {
			versions = append(versions, version)
			modulePath := "example.com/" + module
			modFile := []byte("module " + modulePath + "\n\ngo 1.23.0\n")
			if err := os.WriteFile(filepath.Join(dir, version+".mod"), modFile, 0o600); err != nil {
				t.Fatal(err)
			}
			info := fmt.Sprintf("{\"Version\":%q,\"Time\":%q}\n", version, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339))
			if err := os.WriteFile(filepath.Join(dir, version+".info"), []byte(info), 0o600); err != nil {
				t.Fatal(err)
			}
			zipFile, err := os.Create(filepath.Join(dir, version+".zip"))
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(zipFile)
			body, err := zw.Create(modulePath + "@" + version + "/go.mod")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(body, string(modFile)); err != nil {
				t.Fatal(err)
			}
			body, err = zw.Create(modulePath + "@" + version + "/x.go")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(body, "package "+module+"\n"); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := zipFile.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "list"), []byte(strings.Join(versions, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
