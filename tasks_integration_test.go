package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperStep is a YAML flow list that runs the helper with args.
func helperStep(args ...string) string {
	parts := []string{yamlQuote(helperBin)}
	for _, a := range args {
		parts = append(parts, yamlQuote(a))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func tasksRepo(t *testing.T, config string, files map[string]string) *fixture {
	t.Helper()
	all := map[string]string{".graft.yaml": "schema: 1\nversion:\n  mode: git-tag\n" + config}
	for k, v := range files {
		all[k] = v
	}
	return newRepo(t, all)
}

func (f *fixture) lines(name string) []string {
	f.t.Helper()
	return strings.Split(strings.TrimSpace(f.read(name)), "\n")
}

func (f *fixture) path(name string) string { return filepath.Join(f.work, name) }

func TestTaskDepsRunOnceInOrder(t *testing.T) {
	f := tasksRepo(t, "", nil)
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  a:
    desc: first
    run: [`+helperStep("append", out, "a")+`]
  b:
    deps: [a]
    run: [`+helperStep("append", out, "b")+`]
  c:
    desc: last
    deps: [a, b]
    run:
      - task: b
      - `+helperStep("append", out, "c")+`
`)
	f.mustGraft("c")
	if got := strings.Join(f.lines("out.txt"), ","); got != "a,b,c" {
		t.Errorf("ran %s, want a,b,c", got)
	}
	help := f.mustGraft("help")
	for _, want := range []string{"graft commit", "Tasks", "a  ", "first", "c  ", "last"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
}

func TestTaskArguments(t *testing.T) {
	f := tasksRepo(t, "", nil)
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  echo:
    run: [`+helperStep("append", out, "x", "{args}", "y")+`]
  plain:
    run: [`+helperStep("append", out, "plain")+`]
`)
	f.mustGraft("echo", "--", "1", "two words")
	f.mustGraft("run", "echo")
	if got := f.lines("out.txt"); len(got) != 2 || got[0] != "x 1 two words y" || got[1] != "x y" {
		t.Errorf("lines = %q", got)
	}
	if out, code := f.graft("", "echo", "1"); code != 2 || !strings.Contains(out, "after --") {
		t.Errorf("args without --: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "plain", "--", "1"); code != 1 || !strings.Contains(out, "takes no arguments") {
		t.Errorf("args to a task without {args}: exit %d\n%s", code, out)
	}
	if out, code := f.graft("", "nosuch"); code != 2 || !strings.Contains(out, "unknown command or task") {
		t.Errorf("unknown task: exit %d\n%s", code, out)
	}
}

func TestTaskShellStepQuotesArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh steps need a POSIX shell")
	}
	f := tasksRepo(t, `tasks:
  sh:
    run:
      - sh: printf '%s|' {args} > out.txt
`, nil)
	f.mustGraft("sh", "--", "a b", "it's", "$HOME")
	if got := f.read("out.txt"); got != "a b|it's|$HOME|" {
		t.Errorf("out = %q", got)
	}
}

func TestTaskEnvironment(t *testing.T) {
	f := tasksRepo(t, "", map[string]string{".env": "A=1\nB=2\n"})
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  env:
    dotenv: [A]
    env: {C: '${B}-x'}
    run: [`+helperStep("env", out, "A", "B", "C", "GRAFT_COMMIT", "FROM_SHELL")+`]
`)
	f.env = []string{"GRAFT_COMMIT=1", "FROM_SHELL=yes"}
	f.mustGraft("env")
	want := "A=1,B unset,C=2-x,GRAFT_COMMIT unset,FROM_SHELL=yes"
	if got := strings.Join(f.lines("out.txt"), ","); got != want {
		t.Errorf("env = %s, want %s", got, want)
	}
}

func TestTaskKeepGoing(t *testing.T) {
	f := tasksRepo(t, "", nil)
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  all:
    keep_going: true
    run:
      - `+helperStep("fail")+`
      - `+helperStep("append", out, "after")+`
  halt:
    run:
      - `+helperStep("fail")+`
      - `+helperStep("append", out, "never")+`
`)
	res, code := f.graft("", "all")
	if code != 1 || !strings.Contains(res, "1 of 2 steps failed") {
		t.Errorf("keep_going: exit %d\n%s", code, res)
	}
	if _, code := f.graft("", "halt"); code != 1 {
		t.Errorf("halt: exit %d", code)
	}
	if got := strings.Join(f.lines("out.txt"), ","); got != "after" {
		t.Errorf("ran %s", got)
	}
}

func TestGateRunsTaskSteps(t *testing.T) {
	f := tasksRepo(t, "", nil)
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`gate:
  - task: lint
tasks:
  lint:
    run: [`+helperStep("append", out, "lint")+`]
`)
	f.mustGraft("gate")
	if got := f.read("out.txt"); got != "lint\n" {
		t.Errorf("out = %q", got)
	}
}

func TestTaskLocks(t *testing.T) {
	f := tasksRepo(t, "", nil)
	log := f.path("log.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  slow:
    lock: write
    run: [`+helperStep("log", log, "1500")+`]
  outer:
    lock: write
    run: [{task: inner}]
  inner:
    lock: read
    run: [`+helperStep("ok")+`]
  reader:
    lock: read
    run: [{task: writer}]
  writer:
    lock: {name: work-tree, mode: write}
    run: [`+helperStep("ok")+`]
`)
	f.mustGraft("outer")
	if out, code := f.graft("", "reader"); code != 1 || !strings.Contains(out, "held for reading by an outer task") {
		t.Errorf("read to write upgrade: exit %d\n%s", code, out)
	}

	var wg sync.WaitGroup
	wg.Go(func() { f.mustGraft("slow") })
	waitForFile(t, log)
	locks := f.mustGraft("locks")
	if !strings.Contains(locks, "lock work-tree held by graft slow") {
		t.Errorf("locks while slow runs:\n%s", locks)
	}
	f.mustGraft("slow")
	wg.Wait()
	checkNoOverlap(t, f.read("log.txt"))
	if locks := f.mustGraft("locks"); !strings.Contains(locks, "no locks held") {
		t.Errorf("locks after:\n%s", locks)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", path)
}

// checkNoOverlap parses "start|end pid nanos" lines and fails when two runs
// overlap.
func checkNoOverlap(t *testing.T, log string) {
	t.Helper()
	var open bool
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		event, _, _ := strings.Cut(line, " ")
		switch {
		case event == "start" && open:
			t.Fatalf("runs overlap:\n%s", log)
		case event == "start":
			open = true
		case event == "end":
			open = false
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func listeningOn(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func serviceRepo(t *testing.T, mode string, extra string) (*fixture, int) {
	t.Helper()
	port := freePort(t)
	f := tasksRepo(t, "", map[string]string{".env": fmt.Sprintf("PORT=%d\nSECRET=s\n", port)})
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  build:
    run: [`+helperStep("append", f.path("built.txt"), "built")+`]
services:
  web:
    desc: the web server
    build: build
    run: `+helperStep(mode, "127.0.0.1:${PORT}")+`
    dotenv: [PORT]
    addr: ':${PORT}'
    pidfile: tmp/web.pid
    log: tmp/web.log
`+extra)
	t.Cleanup(func() { f.graft("", "stop") })
	return f, port
}

func TestServiceStartStopRestart(t *testing.T) {
	f, port := serviceRepo(t, "serve", "")
	out := f.mustGraft("start")
	if !strings.Contains(out, "started web") || !listeningOn(port) {
		t.Fatalf("start:\n%s", out)
	}
	if f.read("built.txt") != "built\n" {
		t.Error("build task did not run")
	}
	pid := strings.TrimSpace(f.read("tmp/web.pid"))
	if out := f.mustGraft("start", "web"); !strings.Contains(out, "already running (pid "+pid+")") {
		t.Errorf("second start:\n%s", out)
	}
	status := f.mustGraft("status")
	if !strings.Contains(status, "web: running (pid "+pid+")") || !strings.Contains(status, "listening") {
		t.Errorf("status:\n%s", status)
	}
	if !strings.Contains(f.mustGraft("help"), "the web server") {
		t.Error("help does not list the service")
	}

	f.mustGraft("restart", "web")
	if newPID := strings.TrimSpace(f.read("tmp/web.pid")); newPID == pid || !listeningOn(port) {
		t.Errorf("restart kept pid %s or does not listen", newPID)
	}
	f.mustGraft("stop")
	if _, err := os.Stat(f.path("tmp/web.pid")); !os.IsNotExist(err) || listeningOn(port) {
		t.Errorf("after stop: pid file %v, listening %v", err, listeningOn(port))
	}
	if out := f.mustGraft("stop"); !strings.Contains(out, "web is not running") {
		t.Errorf("second stop:\n%s", out)
	}
	if !strings.Contains(f.read("tmp/web.log"), "listening on 127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("log:\n%s", f.read("tmp/web.log"))
	}
}

func TestServiceStartFailures(t *testing.T) {
	f, port := serviceRepo(t, "fail", "")
	out, code := f.graft("", "start")
	if code != 1 || !strings.Contains(out, "exited before listening") || !strings.Contains(out, "failing on purpose") {
		t.Errorf("dying service: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(f.path("tmp/web.pid")); !os.IsNotExist(err) {
		t.Errorf("pid file left behind: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if out, code := f.graft("", "start"); code != 1 || !strings.Contains(out, "already in use by a process graft did not start") {
		t.Errorf("busy address: exit %d\n%s", code, out)
	}
}

func TestServiceStubbornIsKilled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM on Windows")
	}
	f, port := serviceRepo(t, "stubborn", "    stop_timeout: 1s\n")
	f.mustGraft("start")
	out := f.mustGraft("stop")
	if !strings.Contains(out, "did not stop in 1s, killing it") || listeningOn(port) {
		t.Errorf("stop:\n%s", out)
	}
}

func TestServiceStalePIDFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pid reuse is detected through /proc")
	}
	f, _ := serviceRepo(t, "serve", "")
	// A live process that is not the service: the pid was reused.
	f.write("tmp/web.pid", strconv.Itoa(os.Getpid())+"\n")
	out := f.mustGraft("status")
	if !strings.Contains(out, "removing stale pid file") || !strings.Contains(out, "web: stopped") {
		t.Errorf("status:\n%s", out)
	}
	if _, err := os.Stat(f.path("tmp/web.pid")); !os.IsNotExist(err) {
		t.Errorf("stale pid file kept: %v", err)
	}
}

func TestTools(t *testing.T) {
	f := tasksRepo(t, `tools:
  linter:
    go_install: example.com/linter/cmd/linter@v1.2.0
    check: `+helperStep("print", "rules: fallback-return")+`
    expect: fallback-return
  old:
    go_install: example.com/old@v2.0.0
    check: `+helperStep("print", "old v1.0.0")+`
    expect: v2.0.0
`, nil)
	out, code := f.graft("", "tools")
	if code != 1 || !strings.Contains(out, "linter: ok") || !strings.Contains(out, `does not print "v2.0.0": old v1.0.0`) ||
		!strings.Contains(out, "tools not ready: old") {
		t.Errorf("tools: exit %d\n%s", code, out)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// A go that installs nothing: the check still fails and graft says PATH
	// finds another copy before the install directory.
	fakeBin, gobin := t.TempDir(), t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\ninstall) echo \"$GOBIN $*\" >> " + f.path("go.txt") + " ;;\nenv) echo " + gobin + "; echo /nowhere ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = []string{"PATH=" + fakeBin + string(os.PathListSeparator) + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, code = f.graft("", "tools", "install")
	if code != 1 || !strings.Contains(out, "installed into "+gobin+", but PATH finds "+helperBin+" first") {
		t.Errorf("tools install: exit %d\n%s", code, out)
	}
	if got := f.read("go.txt"); got != gobin+" install example.com/old@v2.0.0\n" {
		t.Errorf("go install calls:\n%s", got)
	}

	// tools.bin_dir from the user config, on PATH ahead of the copy in use:
	// graft warns that the install would shadow it.
	userBin, cfgHome := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgHome, "graft"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "graft", "config.yaml"), []byte("tools:\n  bin_dir: "+userBin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := string(os.PathListSeparator)
	f.env = []string{"XDG_CONFIG_HOME=" + cfgHome, "PATH=" + fakeBin + list + userBin + list + binDir + list + os.Getenv("PATH")}
	out, _ = f.graft("", "tools", "install")
	if !strings.Contains(out, "installing into "+userBin+" puts it on PATH ahead of "+helperBin) {
		t.Errorf("no shadowing warning:\n%s", out)
	}
	if got := f.lines("go.txt"); got[len(got)-1] != userBin+" install example.com/old@v2.0.0" {
		t.Errorf("GOBIN not set to bin_dir: %q", got)
	}
}

// A tool go install cannot provide (a system package) is checked the same
// way; graft prints how to install it and never runs that command.
func TestToolsManual(t *testing.T) {
	f := tasksRepo(t, `tools:
  shellcheck:
    manual: sudo dnf install ShellCheck
    check: `+helperStep("print", "version: 0.10.0")+`
    expect: 'version: 0.11.0'
`, nil)
	out, code := f.graft("", "tools")
	if code != 1 || !strings.Contains(out, `does not print "version: 0.11.0"`) ||
		!strings.Contains(out, "install it yourself: sudo dnf install ShellCheck") {
		t.Errorf("tools: exit %d\n%s", code, out)
	}
	if runtime.GOOS == "windows" {
		return
	}
	fakeBin := t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> " + f.path("go.txt") + "\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = []string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, code = f.graft("", "tools", "install")
	if code != 1 || !strings.Contains(out, "install it yourself: sudo dnf install ShellCheck") ||
		!strings.Contains(out, "tools not ready: shellcheck") {
		t.Errorf("tools install: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(f.path("go.txt")); !os.IsNotExist(err) {
		t.Errorf("go called for a manual tool: %v", err)
	}
}

func TestTestDBRefusesForeignDSN(t *testing.T) {
	f := tasksRepo(t, "", nil)
	f.write(".graft.yaml", f.read(".graft.yaml")+`test_db:
  image: postgres:18-alpine
  container: graft-never-created
  port: 1
  database: app_test
  user: app
  password: app
tasks:
  test:
    test_db: true
    run: [`+helperStep("ok")+`]
`)
	f.env = []string{"TEST_DB_DSN=postgres://app:app@127.0.0.1:5432/app_prod"}
	out, code := f.graft("", "test")
	if code != 1 || !strings.Contains(out, `TEST_DB_DSN points at database "app_prod", not the test database "app_test"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// TestTestDB needs docker and the postgres image; graft test enables it.
func TestTestDB(t *testing.T) {
	if os.Getenv("GRAFT_TEST_DOCKER") != "1" {
		t.Skip("set GRAFT_TEST_DOCKER=1 to run against docker")
	}
	container := fmt.Sprintf("graft-it-testdb-%d", os.Getpid())
	port := freePort(t)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", container).Run() })
	f := tasksRepo(t, "", map[string]string{".env": "OTHER=1\nAPP_DSN=old\n"})
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+fmt.Sprintf(`test_db:
  image: postgres:18-alpine
  container: %s
  port: %d
  database: app_test
  user: app
  password: app
  dsn_var: APP_DSN
  migrate: %s
tasks:
  test:
    test_db: true
    run: [%s]
`, container, port, helperStep("append", out, "migrate", "${APP_DSN}"), helperStep("env", out, "APP_DSN")))
	f.mustGraft("test")
	dsn := fmt.Sprintf("postgres://app:app@127.0.0.1:%d/app_test?sslmode=disable", port)
	// ${APP_DSN} in migrate is the fresh DSN, not the stale one in .env.
	if got := strings.Join(f.lines("out.txt"), ","); got != "migrate "+dsn+",APP_DSN="+dsn {
		t.Errorf("out = %s", got)
	}
	if env := f.read(".env"); env != "OTHER=1\nAPP_DSN="+dsn+"\n" {
		t.Errorf(".env:\n%s", env)
	}
	if st := f.mustGraft("testdb", "status"); !strings.Contains(st, container+" is running") {
		t.Errorf("status:\n%s", st)
	}
	f.mustGraft("testdb", "down")
	if st := f.mustGraft("testdb", "status"); !strings.Contains(st, "does not exist") {
		t.Errorf("status after down:\n%s", st)
	}
}

// TestTestDBMigrateOnFreshClone: without a .env file, ${dsn_var} in migrate
// still resolves to the DSN of the database just brought up.
func TestTestDBMigrateOnFreshClone(t *testing.T) {
	if os.Getenv("GRAFT_TEST_DOCKER") != "1" {
		t.Skip("set GRAFT_TEST_DOCKER=1 to run against docker")
	}
	container := fmt.Sprintf("graft-it-testdb-fresh-%d", os.Getpid())
	port := freePort(t)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", "-v", container).Run() })
	f := tasksRepo(t, "", nil)
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+fmt.Sprintf(`test_db:
  image: postgres:18-alpine
  container: %s
  port: %d
  database: app_test
  user: app
  password: app
  migrate: %s
`, container, port, helperStep("append", out, "migrate", "--dsn=${TEST_DB_DSN}")))
	f.mustGraft("testdb", "up")
	dsn := fmt.Sprintf("postgres://app:app@127.0.0.1:%d/app_test?sslmode=disable", port)
	if got := f.read("out.txt"); got != "migrate --dsn="+dsn+"\n" {
		t.Errorf("migrate got %q", got)
	}
	if _, err := os.Stat(f.path(".env")); !os.IsNotExist(err) {
		t.Errorf("testdb up created .env: %v", err)
	}
}
