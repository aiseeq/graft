package tasks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/stats"
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

// TestCheckGoFmtListsAConflictOnce: git lists a file in a merge conflict once
// per stage; the step prints it once.
func TestCheckGoFmtListsAConflictOnce(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil && args[0] != "merge" {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("go.mod", "module example.com/m\n\ngo 1.26\n")
	write("a.go", "package m\n\nfunc f() {}\n")
	git("add", "-A")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "other")
	write("a.go", "package m\n\nfunc g() {}\n")
	git("commit", "-qam", "other")
	git("checkout", "-q", "main")
	write("a.go", "package m\n\nfunc h() {}\n")
	git("commit", "-qam", "main")
	git("merge", "-q", "other")
	var out bytes.Buffer
	r := &Runner{Root: root, Config: &config.Config{}, Stdout: &out}
	if err := r.checkGoFmt(context.Background(), []string{"."}); err == nil || !strings.Contains(err.Error(), "a.go") {
		t.Fatalf("a file with conflict markers must fail naming it: %v", err)
	}
	if n := strings.Count(out.String(), "a.go"); n != 1 {
		t.Errorf("a.go listed %d times:\n%s", n, out.String())
	}
}

// TestBuiltinStepFailureHasReason: a gate step graft does in its own process
// gives its error as the reason of the failure, which stats --failures shows.
func TestBuiltinStepFailureHasReason(t *testing.T) {
	f := &frame{task: "gate"}
	r := &Runner{frames: []*frame{f}}
	err := r.measureFunc(GoFmtSource, func() error { return errors.New("Go files need formatting: a.go") })
	if err == nil || f.reason != "Go files need formatting: a.go" || f.exit != 1 {
		t.Fatalf("err %v, frame reason %q exit %d", err, f.reason, f.exit)
	}
}

func TestBuiltinGofmtStepIsMarkedInStats(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r := &Runner{
		Root:       t.TempDir(),
		Stats:      stats.NewRecorder(io.Discard),
		frames:     []*frame{{task: stats.GateTask}},
		noRepoName: true,
	}
	err := r.measureFunc(GoFmtSource, func() error {
		until := time.Now().Add(30 * time.Millisecond)
		for time.Now().Before(until) {
			runtime.Gosched()
		}
		return nil
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	records, _, err := stats.Load(path, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one built-in gofmt step", records)
	}
	if !records[0].Builtin || records[0].Step != GoFmtSource {
		t.Fatalf("records = %+v, want one marked built-in gofmt step", records)
	}
	if !records[0].BuiltinCostKnown || records[0].TreeMemMB == nil {
		t.Skip("process CPU or tree memory measurement is unavailable on this platform")
	}
	if records[0].UserS == 0 {
		t.Fatalf("built-in CPU = %g, want a measured process cost", records[0].UserS)
	}
}
