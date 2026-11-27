package app

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gittest"
	"github.com/aiseeq/graft/v2/internal/message"
)

const modulesHoldPath = "example.com/own/lib"

func modulesHoldProject(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projectA")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, root, "init", "-q", "-b", "main")
	gittest.Run(t, root, "config", "user.name", "Test")
	gittest.Run(t, root, "config", "user.email", "test@example.invalid")
	files := map[string]string{
		".graft.yaml":  "schema: 1\nversion: {mode: none}\ngate: []\ngo_vet: {enabled: false, reason: empty gate fixture}\ngo_fmt: {enabled: false, reason: empty gate fixture}\n",
		"go.mod":       "module example.com/projectA\n\ngo 1.24\n\nrequire example.com/own/lib v1.0.0\n",
		"go.sum":       "",
		"main.go":      "package main\n\nimport \"example.com/own/lib\"\n\nfunc main() { _ = lib.Value() }\n",
		"package.json": "{\n  \"dependencies\": {\n    \"own-npm\": \"git+https://github.com/example/own-npm.git#v1.0.0\"\n  }\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, root, "add", ".")
	gittest.Run(t, root, "commit", "-q", "-m", "base")

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := strings.TrimPrefix(r.URL.Path, "/"+modulesHoldPath+"/@v/")
		switch version {
		case "list":
			_, _ = io.WriteString(w, "v1.0.0\nv1.1.0\n")
		case "@latest":
			_, _ = io.WriteString(w, "{\"Version\":\"v1.1.0\",\"Time\":\"2026-01-01T00:00:00Z\"}")
		case "v1.0.0.mod", "v1.1.0.mod":
			_, _ = io.WriteString(w, "module "+modulesHoldPath+"\n\ngo 1.24\n")
		case "v1.0.0.info":
			_, _ = io.WriteString(w, "{\"Version\":\"v1.0.0\",\"Time\":\"2025-01-01T00:00:00Z\"}")
		case "v1.1.0.info":
			_, _ = io.WriteString(w, "{\"Version\":\"v1.1.0\",\"Time\":\"2026-01-01T00:00:00Z\"}")
		case "v1.0.0.zip", "v1.1.0.zip":
			v := strings.TrimSuffix(version, ".zip")
			var data bytes.Buffer
			zw := zip.NewWriter(&data)
			file, err := zw.Create(fmt.Sprintf("%s@%s/lib.go", modulesHoldPath, v))
			if err != nil {
				t.Errorf("create proxy archive: %v", err)
				return
			}
			_, _ = io.WriteString(file, "package lib\n\nfunc Value() int { return 1 }\n")
			if err := zw.Close(); err != nil {
				t.Errorf("close proxy archive: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(data.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(proxy.Close)
	configHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configHome, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "graft", "config.yaml"), []byte("modules:\n  latest: [example.com/own, github.com/example/own-npm]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("GOPROXY", proxy.URL)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOMODCACHE", filepath.Join(t.TempDir(), "gomodcache"))
	return root, proxy
}

func runCommitModuleRaise(t *testing.T, root string, output, warnings *bytes.Buffer) error {
	t.Helper()
	a := &App{Dir: root, Stdout: output, Stderr: warnings}
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	branch, err := repo.Branch()
	if err != nil {
		return err
	}
	return a.raiseCommitModules(context.Background(), repo, cfg, branch)
}

func useNewerModuleTags(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = ls-remote ]; then\n  printf '%s\\n' '0123456789abcdef refs/tags/v1.0.0' 'fedcba9876543210 refs/tags/v1.1.0'\n  exit 0\nfi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAllowEmptyCommitDoesNotRaiseModules(t *testing.T) {
	root, _ := modulesHoldProject(t)
	useNewerModuleTags(t)
	before, _ := moduleFiles(t, root)
	app := &App{Dir: root, Stdout: io.Discard, Stderr: io.Discard}
	err := app.Commit(context.Background(), CommitOptions{
		Message:    message.Source{Parts: []string{"chore: link PROJ-123 source"}},
		AllowEmpty: true,
	})
	if err != nil {
		t.Fatalf("Commit --allow-empty: %v", err)
	}
	after, _ := moduleFiles(t, root)
	if !bytes.Equal(after, before) {
		t.Fatalf("allow-empty changed go.mod:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	git := func(args ...string) string { return gittest.Run(t, root, args...) }
	if tree := git("rev-parse", "HEAD^{tree}"); tree != git("rev-parse", "HEAD^1^{tree}") {
		t.Fatalf("allow-empty module link changed tree: current %s, parent %s", tree, git("rev-parse", "HEAD^1^{tree}"))
	}
}

func moduleFiles(t *testing.T, root string) (goMod, goSum []byte) {
	t.Helper()
	var err error
	goMod, err = os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goSum, err = os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	return goMod, goSum
}

func TestGateModulesHold(t *testing.T) {
	root, _ := modulesHoldProject(t)
	useNewerModuleTags(t)
	beforeMod, beforeSum := moduleFiles(t, root)
	beforePackage, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAFT_MODULES_HOLD", "1")
	var out bytes.Buffer
	if err := (&App{Dir: root, Stdout: &out, Stderr: io.Discard}).Gate(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterMod, afterSum := moduleFiles(t, root)
	if !bytes.Equal(afterMod, beforeMod) || !bytes.Equal(afterSum, beforeSum) {
		t.Fatalf("gate changed module files: go.mod %q -> %q; go.sum %q -> %q", beforeMod, afterMod, beforeSum, afterSum)
	}
	if afterPackage, err := os.ReadFile(filepath.Join(root, "package.json")); err != nil || !bytes.Equal(afterPackage, beforePackage) {
		t.Fatalf("gate changed package.json: %q -> %q, err %v", beforePackage, afterPackage, err)
	}
	if !strings.HasPrefix(out.String(), "graft: modules.latest: held by GRAFT_MODULES_HOLD\n") {
		t.Fatalf("gate output = %q, want held modules line", out.String())
	}
}

func TestGateRaisesModulesWithoutHold(t *testing.T) {
	root, _ := modulesHoldProject(t)
	useNewerModuleTags(t)
	t.Setenv("GRAFT_MODULES_HOLD", "false")
	var out bytes.Buffer
	if err := (&App{Dir: root, Stdout: &out, Stderr: io.Discard}).Gate(context.Background()); err != nil {
		t.Fatal(err)
	}
	goMod, _ := moduleFiles(t, root)
	if !bytes.Contains(goMod, []byte("example.com/own/lib v1.1.0")) {
		t.Fatalf("go.mod = %q, want raised module v1.1.0", goMod)
	}
	packageJSON, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(packageJSON, []byte("#v1.1.0")) {
		t.Fatalf("package.json = %q, want raised npm git tag v1.1.0", packageJSON)
	}
}

func TestGateRejectsInvalidModulesHold(t *testing.T) {
	root, _ := modulesHoldProject(t)
	t.Setenv("GRAFT_MODULES_HOLD", "not-a-bool")
	var out bytes.Buffer
	if err := (&App{Dir: root, Stdout: &out, Stderr: io.Discard}).Gate(context.Background()); err == nil || !strings.Contains(err.Error(), "GRAFT_MODULES_HOLD") {
		t.Fatalf("gate error = %v, want invalid GRAFT_MODULES_HOLD", err)
	}
	if out.Len() != 0 {
		t.Fatalf("gate output = %q, want empty", out.String())
	}
	goMod, goSum := moduleFiles(t, root)
	if !bytes.Equal(goMod, []byte("module example.com/projectA\n\ngo 1.24\n\nrequire example.com/own/lib v1.0.0\n")) || len(goSum) != 0 {
		t.Fatalf("invalid setting changed modules: go.mod %q, go.sum %q", goMod, goSum)
	}
}

func TestCommitIgnoresInvalidModulesHold(t *testing.T) {
	root, _ := modulesHoldProject(t)
	useNewerModuleTags(t)
	t.Setenv("GRAFT_MODULES_HOLD", "not-a-bool")
	var out, stderr bytes.Buffer
	if err := runCommitModuleRaise(t, root, &out, &stderr); err != nil {
		t.Fatalf("commit module raise: %v\nstdout: %s\nstderr: %s", err, out.String(), stderr.String())
	}
	goMod, _ := moduleFiles(t, root)
	if !bytes.Contains(goMod, []byte("example.com/own/lib v1.1.0")) {
		t.Fatalf("commit go.mod = %q, want raised module v1.1.0", goMod)
	}
	if !strings.Contains(stderr.String(), "graft: warning: GRAFT_MODULES_HOLD is ignored by commit\n") {
		t.Fatalf("commit warning = %q", stderr.String())
	}
}
