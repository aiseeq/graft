package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeProxy is a file:// Go module proxy serving example.com/fake at the
// versions given; the program prints its version.
func fakeProxy(t *testing.T, versions ...string) string {
	t.Helper()
	dir := t.TempDir()
	at := filepath.Join(dir, "example.com", "fake", "@v")
	writeFile(t, filepath.Join(at, "list"), strings.Join(versions, "\n")+"\n")
	for _, v := range versions {
		mod := "module example.com/fake\n\ngo 1.21\n"
		writeFile(t, filepath.Join(at, v+".info"), `{"Version":"`+v+`","Time":"2026-01-01T00:00:00Z"}`)
		writeFile(t, filepath.Join(at, v+".mod"), mod)
		f, err := os.Create(filepath.Join(at, v+".zip"))
		if err != nil {
			t.Fatal(err)
		}
		z := zip.NewWriter(f)
		for name, content := range map[string]string{
			"go.mod":  mod,
			"main.go": "package main\n\nfunc main() { println(\"fake " + v + "\") }\n",
		} {
			w, err := z.Create("example.com/fake@" + v + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestDeployRunsPinnedTools: a tool release newer than the project's pin
// must not stop a deploy. The deps and the deploy script find the pinned
// build first on PATH, installed once into graft's cache.
func TestDeployRunsPinnedTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	proxy := fakeProxy(t, "v1.0.0", "v1.1.0")
	state := t.TempDir()
	newer := t.TempDir()
	writeFile(t, filepath.Join(newer, "fake"), "#!/bin/sh\necho fake from PATH, newer than the pin >&2\n")
	if err := os.Chmod(filepath.Join(newer, "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
envs:
  local: {}
tools:
  fake: {go_install: example.com/fake@v1.0.0}
tasks:
  lint:
    run: [[sh, -c, 'fake 2> ` + filepath.Join(state, "lint") + `']]
deploy:
  remote: origin
  targets:
    test:
      env: local
      deps: [lint]
      run: [sh, -c, 'fake 2> ` + filepath.Join(state, "script") + `']
`,
	}, "origin")
	cache := t.TempDir()
	f.env = append(f.env,
		"GOPROXY=file://"+filepath.ToSlash(proxy), "GOSUMDB=off", "GOFLAGS=-modcacherw", "GOMODCACHE="+t.TempDir(),
		"XDG_CACHE_HOME="+cache, "PATH="+newer+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.mustGraft("commit", "-m", "feat: base")

	out := f.mustGraft("deploy", "test")
	for name, file := range map[string]string{"deps": "lint", "deploy script": "script"} {
		if got := strings.TrimSpace(readFile(t, filepath.Join(state, file))); got != "fake v1.0.0" {
			t.Errorf("the %s ran %q, not the pinned build\n%s", name, got, out)
		}
	}
	if !strings.Contains(out, "go install example.com/fake@v1.0.0") || !strings.Contains(out, "pinned tools first on PATH: fake v1.0.0") {
		t.Errorf("no install or no note:\n%s", out)
	}
	if out := f.mustGraft("deploy", "test"); strings.Contains(out, "go install") {
		t.Errorf("installed the pinned build again:\n%s", out)
	}

	// A pin that cannot be installed stops the deploy before deps and the
	// script, never falling back to the copy on PATH.
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "fake@v1.0.0", "fake@v9.9.9", 1))
	f.mustGraft("commit", "-m", "chore: pin a missing release")
	if err := os.Remove(filepath.Join(state, "script")); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "deploy", "test")
	if code == 0 || !strings.Contains(out, "deploy test: nothing was deployed: the pinned fake example.com/fake@v9.9.9") {
		t.Errorf("a missing pin: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(state, "script")); err == nil {
		t.Error("the deploy script ran")
	}
}
