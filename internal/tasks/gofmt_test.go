package tasks

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestCheckGoFmt(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"module/go.mod":               "module example.com/module\n\ngo 1.26\n",
		"module/good.go":              "package p\r\n\r\nfunc f() {}\r\n",
		"module/bad.go":               "package p\n\nfunc f()  {}\n",
		"module/parse.go":             "package p\nfunc (\n",
		"module/nopkg.go":             "func f() {}\n",
		"module/gone.go":              "package p\nfunc f()  {}\n",
		"module/old/go.mod":           "module example.com/module/old\n\ngo 1.26\n",
		"module/old/x.go":             "package p\nfunc f()  {}\n",
		"module/testdata/skip.go":     "package p\nfunc f()  {}\n",
		"module/vendor/skip.go":       "package p\nfunc f()  {}\n",
		"module/node_modules/skip.go": "package p\nfunc f()  {}\n",
		"module/generated/skip.go":    "package p\nfunc f()  {}\n",
		"module/inner/go.mod":         "module example.com/module/inner\n\ngo 1.26\n",
		"module/inner/x.go":           "package p\nfunc f()  {}\n",
		"outside/unmodded.go":         "package p\nfunc f()  {}\n",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Deleted in the work tree, still in the index: git add -A of a commit
	// stages the deletion, and a plain gate has nothing to check.
	// A go.mod deleted the same way leaves its files to the module above.
	for _, name := range []string{"gone.go", filepath.Join("old", "go.mod")} {
		if err := os.Remove(filepath.Join(root, "module", name)); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{GoVet: config.GoVet{Exclude: []string{"module/generated"}}, GoFmt: config.GoFmt{Enabled: new(true)}}, Stdout: &out}
	err := r.checkGoFmt(context.Background(), []string{"module"})
	if err == nil {
		t.Fatal("unformatted and unparsable files must fail")
	}
	got := out.String()
	for _, name := range []string{"module/bad.go", "module/parse.go: ", "module/nopkg.go: ", "module/old/x.go"} {
		if !strings.Contains(got, name) {
			t.Errorf("missing %s in %q", name, got)
		}
	}
	for _, name := range []string{"module/gone.go", "module/good.go", "module/testdata/skip.go", "module/vendor/skip.go", "module/node_modules/skip.go", "module/generated/skip.go", "outside/unmodded.go"} {
		if strings.Contains(got, name) {
			t.Errorf("unexpected %s in %q", name, got)
		}
	}
	if strings.Contains(got, "module/inner/x.go") {
		t.Errorf("file in the unselected nested module was listed: %q", got)
	}
}
