package crashlog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const crashTestEnv = "GRAFT_TEST_CRASH"

// crashChild runs this test binary as the child of a crash log test, with
// its own cache directory, and returns the child's output and crash.log.
func crashChild(t *testing.T, test, mode string) (string, string, error) {
	t.Helper()
	cache := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), crashTestEnv+"="+mode, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache, "HOME="+cache)
	out, runErr := cmd.CombinedOutput()
	var path string
	for line := range strings.SplitSeq(string(out), "\n") {
		if p, ok := strings.CutPrefix(line, "crash log at "); ok {
			path = p
		}
	}
	if path == "" {
		t.Fatalf("the child did not name its crash log (%v):\n%s", runErr, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the crash log: %v\n%s", err, out)
	}
	return string(out), string(data), runErr
}

// crashChildSetup is the child's start, as graft's main: crash.log opened and the
// note deferred by the caller.
func crashChildSetup() *os.File {
	path, err := Path()
	if err != nil {
		fmt.Println("crash log path:", err)
		os.Exit(3)
	}
	fmt.Println("crash log at " + path)
	return Keep()
}

// TestPanicTextReachesCrashLog: a panic of graft's main goroutine is kept
// whole in crash.log, after a line naming the run.
func TestPanicTextReachesCrashLog(t *testing.T) {
	if os.Getenv(crashTestEnv) == "panic" {
		defer Note(crashChildSetup(), "v0.0.0-test", []string{"commit", "-m", "a message"})
		panic("crash log test")
	}
	out, log, err := crashChild(t, "TestPanicTextReachesCrashLog", "panic")
	if err == nil {
		t.Fatalf("the child did not crash:\n%s", out)
	}
	for _, want := range []string{
		"graft v0.0.0-test, pid ",
		": graft commit -m a message\n",
		"panic: crash log test",
		"TestPanicTextReachesCrashLog",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("crash.log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(out, "panic: crash log test") {
		t.Errorf("stderr lost the panic:\n%s", out)
	}
}

// TestGoroutinePanicReachesCrashLog: a panic of another goroutine, which
// Note does not see, still reaches crash.log.
func TestGoroutinePanicReachesCrashLog(t *testing.T) {
	if os.Getenv(crashTestEnv) == "goroutine" {
		defer Note(crashChildSetup(), "v0.0.0-test", []string{"gate"})
		done := make(chan struct{})
		go func() {
			defer close(done)
			panic("goroutine crash log test")
		}()
		<-done
		return
	}
	out, log, err := crashChild(t, "TestGoroutinePanicReachesCrashLog", "goroutine")
	if err == nil {
		t.Fatalf("the child did not crash:\n%s", out)
	}
	if !strings.Contains(log, "panic: goroutine crash log test") {
		t.Errorf("crash.log lacks the goroutine's panic:\n%s", log)
	}
}

// TestUnreachableCacheKeepsNoCrashLog: a cache directory that cannot be
// made leaves Keep without a file, and the caller runs on.
func TestUnreachableCacheKeepsNoCrashLog(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"XDG_CACHE_HOME", "LOCALAPPDATA", "HOME"} {
		t.Setenv(name, notDir)
	}
	if f := Keep(); f != nil {
		name := f.Name()
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		t.Fatalf("Keep opened %s under a file", name)
	}
}

// TestCalmRunWritesNoCrashLog: a run that does not crash leaves crash.log
// empty.
func TestCalmRunWritesNoCrashLog(t *testing.T) {
	if os.Getenv(crashTestEnv) == "calm" {
		defer Note(crashChildSetup(), "v0.0.0-test", []string{"help"})
		return
	}
	out, log, err := crashChild(t, "TestCalmRunWritesNoCrashLog", "calm")
	if err != nil {
		t.Fatalf("the calm child failed: %v\n%s", err, out)
	}
	if log != "" {
		t.Errorf("a calm run wrote crash.log:\n%s", log)
	}
}
