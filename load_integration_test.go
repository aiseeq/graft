package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestHeavyTaskWaitsForLoad: a heavy task starts only once the load is below
// the user's wait threshold, gets the user's thread count as {threads}, and a
// task that is not heavy never waits.
func TestHeavyTaskWaitsForLoad(t *testing.T) {
	state := t.TempDir()
	out := filepath.Join(state, "out")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: none}
tasks:
  build:
    heavy: true
    run: [[sh, -c, 'echo threads={threads} env=$GRAFT_THREADS >> ` + out + `']]
  light:
    run: [[sh, -c, 'echo light >> ` + out + `']]
`,
	})
	loadavg := filepath.Join(state, "loadavg")
	writeFile(t, loadavg, "30.50 20.00 10.00 3/400 1234\n")
	cfgDir := t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "load: {threads: 6, poll: 20ms, settle: 10ms}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "GRAFT_LOADAVG="+loadavg, "XDG_CACHE_HOME="+t.TempDir())

	read := func() string {
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	f.mustGraft("light")
	if got := read(); got != "light\n" {
		t.Fatalf("a light task under load: %q", got)
	}

	cmd := exec.Command(graftBin, "build")
	cmd.Dir = f.work
	cmd.Env = append(slices.Clone(testEnv), f.env...)
	log := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(log.String(), "waiting") })
	if got := read(); got != "light\n" {
		t.Errorf("the heavy task ran at load 30.50: %q", got)
	}
	writeFile(t, loadavg, "17.00 20.00 10.00 3/400 1234\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	if got := read(); got != "light\nthreads=6 env=6\n" {
		t.Errorf("after the load fell: %q\n%s", got, log.String())
	}
	if !strings.Contains(log.String(), "graft: build: sh -c echo threads=6 ") {
		t.Errorf("the step is echoed without its thread count:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "build: load 30.50 is above 22, waiting") || !strings.Contains(log.String(), "build: load 17.00, starting after ") {
		t.Errorf("no wait reported:\n%s", log.String())
	}
}
