package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestExecTestDB(t *testing.T) {
	if help, code := newRepo(t, nil).graft("", "exec", "-h"); code != 0 || !strings.Contains(help, "--test-db") {
		t.Errorf("exec help omits --test-db: exit %d\n%s", code, help)
	}
	dbConfig := "test_db: {image: postgres:16-alpine, container: projecta-test, port: 5432, database: projecta_test, user: projecta, password: projecta}\n"
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n" + dbConfig})
	dsn := "postgres://projecta:projecta@localhost/projecta_test"
	f.env = append(f.env, "TEST_DB_DSN="+dsn)
	cmd := exec.Command(graftBin, "exec", "--heavy", "--test-db", "--", "sh", "-c", "echo \"$TEST_DB_DSN\"")
	cmd.Dir, cmd.Env = f.work, append(slices.Clone(testEnv), f.env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec: %v\nstderr: %s", err, stderr.String())
	}
	if stdout.String() != dsn+"\n" {
		t.Errorf("stdout = %q, want only DSN", stdout.String())
	}

	missing := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	if out, code := missing.graft("", "exec", "--heavy", "--test-db", "--", "true"); code != 125 || !strings.Contains(out, "exec --test-db: the config has no test_db section") {
		t.Errorf("missing test_db: exit %d\n%s", code, out)
	}
}

func TestExecTestDBStaysHere(t *testing.T) {
	f, state := remoteRepo(t, 8, "")
	calm(t, state)
	f.write(".graft.yaml", strings.Replace(f.read(".graft.yaml"), "tasks:\n", "test_db: {image: postgres:16-alpine, container: projecta-test, port: 5432, database: projecta_test, user: projecta, password: projecta}\ntasks:\n", 1))
	f.env = append(f.env, "TEST_DB_DSN=postgres://projecta:projecta@localhost/projecta_test")
	out := f.mustGraft("exec", "--heavy", "--test-db", "--", "true")
	if !strings.Contains(out, "heavy, on this machine: the task uses the test database") {
		t.Errorf("exec did not report why it stayed here:\n%s", out)
	}
	if calls, err := os.ReadFile(filepath.Join(state, "calls")); (err != nil && !errors.Is(err, os.ErrNotExist)) || len(calls) != 0 {
		t.Errorf("remote command called: %q, %v", calls, err)
	}
}

// TestExecTestDBBringsUpTheDatabase: with no DSN in the environment graft
// exec --test-db brings the container up as a test_db task does and hands
// the command its DSN, the command's stdout left as it is.
func TestExecTestDBBringsUpTheDatabase(t *testing.T) {
	if os.Getenv("GRAFT_TEST_DOCKER") != "1" {
		t.Skip("set GRAFT_TEST_DOCKER=1 to run against docker")
	}
	container := testDBContainer(t, "graft-it-exectestdb")
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n" + fmt.Sprintf(`test_db:
  image: postgres:18-alpine
  container: %s
  socket: true
  database: app_test
  user: app
  password: app
  dsn_var: APP_DSN
`, container)})
	cache := shortCache(t)
	f.env = append(f.env, "XDG_CACHE_HOME="+cache, "LOCALAPPDATA="+cache)
	cmd := exec.Command(graftBin, "exec", "--heavy", "--test-db", "--", "sh", "-c", `echo "dsn=$APP_DSN"`)
	cmd.Dir = f.work
	for _, kv := range append(slices.Clone(testEnv), f.env...) {
		if !strings.HasPrefix(kv, "APP_DSN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec: %v\nstderr: %s", err, stderr.String())
	}
	if want := "dsn=" + socketDSN(cache, container) + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q\nstderr: %s", stdout.String(), want, stderr.String())
	}
	f.mustGraft("testdb", "down")
}
