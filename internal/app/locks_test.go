package app

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

func TestLocksShowsLoadQueueQuietWait(t *testing.T) {
	cache := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	quietDir := filepath.Join(state, "akit")
	if err := os.MkdirAll(quietDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quietDir, "quiet-until"), []byte(time.Now().Add(time.Hour).Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	loadLockPath := filepath.Join(cache, "graft", "load.lock")
	holder, err := lock.TryAcquire(loadLockPath, "graft exec (starting a heavy task)")
	if err != nil {
		t.Fatal(err)
	}
	if holder == nil {
		t.Fatal("could not acquire synthetic load lock")
	}
	defer holder.Release()

	var output bytes.Buffer
	a := &App{Dir: root, Stdout: &output}
	if err := a.Locks(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "waits: quiet mode until") {
		t.Fatalf("graft locks output = %q, want quiet queue reason", output.String())
	}
}

func TestLocksReportsLoadMeasureErrorAndContinues(t *testing.T) {
	cache := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	quietDir := filepath.Join(state, "akit")
	if err := os.MkdirAll(quietDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quietDir, "quiet-until"), []byte("not a timestamp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	loadDir := filepath.Join(cache, "graft")
	loadHolder, err := lock.TryAcquire(filepath.Join(loadDir, "load.lock"), "load head")
	if err != nil || loadHolder == nil {
		t.Fatalf("acquire load lock: holder=%v err=%v", loadHolder, err)
	}
	defer loadHolder.Release()
	settleHolder, err := lock.TryAcquire(filepath.Join(loadDir, "load.settle.lock"), "settler")
	if err != nil || settleHolder == nil {
		t.Fatalf("acquire settle lock: holder=%v err=%v", settleHolder, err)
	}
	defer settleHolder.Release()

	var output bytes.Buffer
	a := &App{Dir: root, Stdout: &output}
	if err := a.Locks(); err != nil {
		t.Fatalf("Locks returned an error: %v\n%s", err, output.String())
	}
	for _, want := range []string{
		"graft: the load queue: cannot measure the machine:",
		"lock load held by load head",
		"lock load.settle held by settler",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("graft locks output = %q, want %q", output.String(), want)
		}
	}
}
