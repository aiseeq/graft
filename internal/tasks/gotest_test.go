package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTrimRunReadsGoEnvAndQuotedGOFLAGS(t *testing.T) {
	dir, _, env := setGoEnvFlags(t, "-trimpath=false")
	got, _, err := trimRun(context.Background(), dir, []string{"go", "test", "example.com/projecta/projectA"}, env, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "-trimpath") {
		t.Fatalf("go env -w GOFLAGS=-trimpath=false: got %q", got)
	}
	got, _, err = trimRun(context.Background(), dir, []string{"go", "test", "example.com/projecta/projectA"}, append(env, `GOFLAGS="-trimpath=false"`), false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "-trimpath") {
		t.Fatalf("quoted GOFLAGS: got %q", got)
	}
}

func TestTrimRunPreservesGoEnvStderr(t *testing.T) {
	fakeBin := t.TempDir()
	goPath := filepath.Join(fakeBin, "go")
	if err := os.WriteFile(goPath, []byte("#!/bin/sh\necho 'synthetic go env failure' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, _, err := trimRun(context.Background(), t.TempDir(), []string{"go", "build", "."}, []string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}, false)
	if err == nil || !strings.Contains(err.Error(), "synthetic go env failure") {
		t.Fatalf("trimRun error = %v, want go env stderr", err)
	}
}

func TestTrimRunReadsMultilineGOFLAGSFromJSON(t *testing.T) {
	dir := t.TempDir()
	env := []string{"GOFLAGS=-count=1\n-trimpath=false"}
	for _, key := range []string{"PATH", "HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	got, _, err := trimRun(context.Background(), dir, []string{"go", "test", "example.com/projecta/projectA"}, env, false)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "-trimpath") {
		t.Fatalf("multiline GOFLAGS: got argv %q", got)
	}
}

func setGoEnvFlags(t *testing.T, flags string) (dir, goenv string, env []string) {
	t.Helper()
	dir = t.TempDir()
	goenv = filepath.Join(dir, "go.env")
	setup := exec.Command("go", "env", "-w", "GOFLAGS="+flags)
	setup.Env = []string{"GOENV=" + goenv}
	for _, key := range []string{"PATH", "HOME", "GOROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			setup.Env = append(setup.Env, key+"="+value)
		}
	}
	if out, err := setup.CombinedOutput(); err != nil {
		t.Fatalf("go env -w GOFLAGS: %v: %s", err, out)
	}
	env = []string{"GOENV=" + goenv}
	for _, key := range []string{"PATH", "HOME", "GOROOT"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return dir, goenv, env
}

func trimForTest(t *testing.T, argv, env, commands []string) []string {
	t.Helper()
	flagsText, _ := lookupEnv(env, "GOFLAGS")
	flags, err := splitGoFlags(flagsText)
	if err != nil {
		t.Fatal(err)
	}
	return addTrimPath(argv, flags, commands)
}

func TestGoPackages(t *testing.T) {
	for _, c := range []struct {
		words []string
		want  []int
	}{
		{[]string{"go", "test", "-p", "4", "-json", "{args}", "./..."}, []int{6}},
		{[]string{"-v", "./cmd/x/", "-run=TestA", "."}, []int{1, 3}},
		{[]string{"-run", "./x", "-timeout", "20m"}, nil},
		{[]string{"-race", "./a", "-args", "./b"}, []int{1}},
		{[]string{"-count=1", "example.com/m/p"}, []int{1}},
	} {
		if got := goPackages(c.words); !slices.Equal(got, c.want) {
			t.Errorf("goPackages(%q) = %v, want %v", c.words, got, c.want)
		}
	}
}

func TestOwnPackagesKeepsOtherSteps(t *testing.T) {
	for _, argv := range [][]string{
		{"env", "A=1", "go", "test", "{args}", "./..."},
		{"go", "test", "./..."},
		{"go", "vet", "{args}", "./..."},
	} {
		if got, dropped := ownPackages(argv, []string{"./cmd/x"}); !slices.Equal(got, argv) || dropped != nil {
			t.Errorf("ownPackages(%q) = %q, dropped %q", argv, got, dropped)
		}
	}
}

func TestTrimPaths(t *testing.T) {
	for _, c := range []struct {
		argv, env, want []string
	}{
		{[]string{"go", "test", "./..."}, nil, []string{"go", "test", "-trimpath", "./..."}},
		{[]string{"go", "vet", "./..."}, nil, []string{"go", "vet", "./..."}},
		{[]string{"go", "build", "--trimpath", "."}, nil, []string{"go", "build", "--trimpath", "."}},
		{[]string{"go", "run", "."}, []string{"GOFLAGS=-p=2 -trimpath=false"}, []string{"go", "run", "."}},
		{[]string{"go", "run", "."}, []string{"GOFLAGS=-trimpath", "GOFLAGS=-p=2"}, []string{"go", "run", "-trimpath", "."}},
		{[]string{"go", "test", "-args", "-trimpath"}, nil, []string{"go", "test", "-trimpath", "-args", "-trimpath"}},
		{[]string{"gotest", "test"}, nil, []string{"gotest", "test"}},
	} {
		if got := trimForTest(t, c.argv, c.env, trimBuilds); !slices.Equal(got, c.want) {
			t.Errorf("addTrimPath build(%q, %q) = %q, want %q", c.argv, c.env, got, c.want)
		}
	}
}

func TestTrimVetPaths(t *testing.T) {
	for _, c := range []struct {
		argv, env, want []string
	}{
		{[]string{"go", "vet", "./..."}, nil, []string{"go", "vet", "-trimpath", "./..."}},
		{[]string{"go", "vet", "-trimpath=false", "./..."}, nil, []string{"go", "vet", "-trimpath=false", "./..."}},
		{[]string{"go", "vet", "./..."}, []string{"GOFLAGS=-tags=x -trimpath=false"}, []string{"go", "vet", "./..."}},
		{[]string{"go", "vet", "./..."}, []string{"GOFLAGS=-trimpath"}, []string{"go", "vet", "./..."}},
		{[]string{"go", "build", "./..."}, nil, []string{"go", "build", "./..."}},
	} {
		if got := trimForTest(t, c.argv, c.env, []string{"vet"}); !slices.Equal(got, c.want) {
			t.Errorf("addTrimPath vet(%q, %q) = %q, want %q", c.argv, c.env, got, c.want)
		}
	}
}
