package tasks

import (
	"os"
	"path/filepath"
	"testing"
)

// The overlay of a go test run: the command line wins over GOFLAGS, an
// inherited environment is read when the step sets none, and the words after
// -args belong to the test binary.
func TestGoTestOverlayFlagSources(t *testing.T) {
	dir := t.TempDir()
	for name, original := range map[string]string{"cmdline.json": "a.go", "goflags.json": "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"Replace":{"`+original+`":""}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOFLAGS", "-overlay=goflags.json")
	for _, c := range []struct {
		name  string
		words []string
		env   []string
		want  string
	}{
		{"command line over GOFLAGS", []string{"test", "-overlay", "cmdline.json", "./..."}, []string{"GOFLAGS=-overlay=goflags.json"}, "a.go"},
		{"GOFLAGS of the step", []string{"test", "./..."}, []string{"GOFLAGS=-overlay=goflags.json"}, "b.go"},
		{"inherited GOFLAGS", []string{"test", "./..."}, nil, "b.go"},
		{"after -args", []string{"test", "./...", "-args", "-overlay=cmdline.json"}, []string{"GOFLAGS="}, ""},
	} {
		files, found, err := goTestOverlay(c.words, c.env, dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := ""
		if found && len(files) == 1 {
			got = files[0].path
		}
		if got != c.want {
			t.Errorf("%s: overlay of %q, found %v; want %q", c.name, got, found, c.want)
		}
	}
}

// GOFLAGS written by go env -w names the overlay too.
func TestGoTestOverlayFromGoEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "goenv.json"), []byte(`{"Replace":{"c.go":""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	goenv := filepath.Join(dir, "go.env")
	if err := os.WriteFile(goenv, []byte("GOFLAGS=-overlay="+filepath.Join(dir, "goenv.json")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOENV", goenv)
	files, found, err := goTestOverlay([]string{"test", "./..."}, nil, dir)
	if err != nil || !found || len(files) != 1 || files[0].path != "c.go" {
		t.Fatalf("overlay %+v, found %v, %v; want c.go from the go env file", files, found, err)
	}
}

// The contents an overlay puts in place, not only its paths, make the tree.
func TestOverlayTreeIDHasTheContents(t *testing.T) {
	a := overlayTreeID("tree", []overlayFile{{path: "a.go", content: "package a // 1"}})
	b := overlayTreeID("tree", []overlayFile{{path: "a.go", content: "package a // 2"}})
	del := overlayTreeID("tree", []overlayFile{{path: "a.go", deleted: true}})
	if a == b || a == del || a == "tree" {
		t.Fatalf("overlay tree ids %s %s %s; want each its own", a, b, del)
	}
}
