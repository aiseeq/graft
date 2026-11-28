package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
)

// loggingSudo is fakeSudo that also logs each call's arguments in the file
// it returns.
func loggingSudo(t *testing.T, body string) string {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls")
	fakeSudo(t, "", `echo "$*" >> "$CALLS"`+"\n"+body)
	t.Setenv("CALLS", calls)
	return calls
}

// listingSudo allows, under -l, every command, printing it as sudo does;
// any command it is asked to run it refuses.
const listingSudo = `[ "$1" = -n ] && shift
[ "$1" = -l ] && { shift; echo "/usr/bin/$*"; exit 0; }
echo "sudo: command not allowed" >&2; exit 1
`

func sudoRecord(t *testing.T, dir string) (*envs.Env, *config.DeployedSHA) {
	t.Helper()
	return &envs.Env{Name: "test", Root: t.TempDir()}, &config.DeployedSHA{Path: filepath.Join(dir, "DEPLOYED_SHA.candidate"), Sudo: true}
}

// Under sudo the check asks sudo -n -l about the write's own command, tee of
// the same path, and runs nothing else under sudo: a rule that allows tee
// but no shell passes.
func TestCheckWritableAsksSudoAboutTheWriteCommand(t *testing.T) {
	calls := loggingSudo(t, listingSudo)
	env, rec := sudoRecord(t, t.TempDir())
	if err := CheckWritable(context.Background(), env, rec); err != nil {
		t.Fatalf("CheckWritable: %v", err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(b)), "-n -l "+writeCommand(rec); got != want || want != "-n -l tee "+rec.Path {
		t.Fatalf("sudo calls %q, want %q", got, want)
	}
	if _, err := os.Stat(rec.Path); !os.IsNotExist(err) {
		t.Fatalf("the check wrote %s: %v", rec.Path, err)
	}
}

// sudo -l refuses the write's command: the record cannot be written.
func TestCheckWritableRefusesWhatSudoDoesNotAllow(t *testing.T) {
	loggingSudo(t, `[ "$1" = -n ] && shift; [ "$1" = -l ] && exit 1; "$@"`)
	env, rec := sudoRecord(t, t.TempDir())
	err := CheckWritable(context.Background(), env, rec)
	if err == nil || errors.Is(err, ErrSudoPassword) || !strings.Contains(err.Error(), "sudo does not allow tee "+rec.Path) {
		t.Fatalf("CheckWritable: %v", err)
	}
}

func TestCheckWritableNamesSudoWantingPassword(t *testing.T) {
	loggingSudo(t, "echo 'sudo: a password is required' >&2; exit 1\n")
	env, rec := sudoRecord(t, t.TempDir())
	if err := CheckWritable(context.Background(), env, rec); !errors.Is(err, ErrSudoPassword) {
		t.Fatalf("CheckWritable: %v", err)
	}
}

// Root writes into any directory there is, not into one that is missing.
func TestCheckWritableUnderSudoRefusesMissingDirectory(t *testing.T) {
	loggingSudo(t, listingSudo)
	env, rec := sudoRecord(t, filepath.Join(t.TempDir(), "missing"))
	err := CheckWritable(context.Background(), env, rec)
	if err == nil || errors.Is(err, ErrDirUnseen) || !strings.Contains(err.Error(), "its directory does not exist") {
		t.Fatalf("CheckWritable: %v", err)
	}
}

// A directory inside one the deploy user cannot enter may exist or not:
// the check cannot tell, and says so.
func TestCheckWritableUnderSudoNamesUnseenDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root enters any directory")
	}
	loggingSudo(t, listingSudo)
	_, locked := lockedSHA(t, false)
	env, rec := sudoRecord(t, filepath.Join(filepath.Dir(locked.Path), "inner"))
	if err := CheckWritable(context.Background(), env, rec); !errors.Is(err, ErrDirUnseen) {
		t.Fatalf("CheckWritable: %v", err)
	}
}

// fakeFindmnt puts a findmnt on PATH that runs body: the mount information
// a test cannot make with a real read-only mount.
func fakeFindmnt(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "findmnt"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The path, when it exists, must be a regular file, with sudo and without:
// graft writes through no link, dangling, to /proc or to a writable file
// alike, nor into a directory. A regular file the write may change passes.
func TestCheckWritableRefusesWhatIsNoRegularFile(t *testing.T) {
	loggingSudo(t, listingSudo)
	for _, sudo := range []bool{true, false} {
		for _, tc := range []struct {
			name, want string
			make       func(path string) error
		}{
			{"link to /proc", "it is a symbolic link", func(path string) error { return os.Symlink("/proc/version", path) }},
			{"link to a writable file", "it is a symbolic link", func(path string) error {
				target := path + ".target"
				if err := os.WriteFile(target, nil, 0o644); err != nil {
					return err
				}
				return os.Symlink(target, path)
			}},
			{"dangling link", "it is a symbolic link", func(path string) error {
				return os.Symlink(filepath.Join(filepath.Dir(path), "missing", "record"), path)
			}},
			{"directory", "it is not a regular file", func(path string) error { return os.Mkdir(path, 0o755) }},
		} {
			t.Run(tc.name+map[bool]string{true: " under sudo", false: " without sudo"}[sudo], func(t *testing.T) {
				env, rec := sudoRecord(t, t.TempDir())
				rec.Sudo = sudo
				if err := tc.make(rec.Path); err != nil {
					t.Fatal(err)
				}
				if err := CheckWritable(context.Background(), env, rec); err == nil || !strings.Contains(err.Error(), "cannot be written") || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("CheckWritable: %v", err)
				}
			})
		}
		env, rec := sudoRecord(t, t.TempDir())
		rec.Sudo = sudo
		mode := os.FileMode(0o644)
		if sudo {
			mode = 0o444
		}
		if err := os.WriteFile(rec.Path, nil, mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckWritable(context.Background(), env, rec); err != nil {
			t.Fatalf("CheckWritable of a regular file the write may change (sudo %v): %v", sudo, err)
		}
	}
}

// The directory's mount is read-only: root cannot write there either. Only
// the ro option counts, not one that merely names it.
func TestCheckWritableUnderSudoReadsTheMountOfTheDirectory(t *testing.T) {
	loggingSudo(t, listingSudo)
	for _, tc := range []struct {
		name, findmnt string
		check         func(error) bool
	}{
		{"read-only", `[ "$*" = "-n -o OPTIONS -T $DIR" ] || exit 2; echo ro,relatime`, func(err error) bool {
			return err != nil && strings.Contains(err.Error(), "is on a file system mounted read-only")
		}},
		{"read-write", `echo rw,relatime,errors=remount-ro`, func(err error) bool { return err == nil }},
		{"unknown", `echo "findmnt: cannot tell" >&2; exit 1`, func(err error) bool { return errors.Is(err, ErrMountUnknown) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DIR", dir)
			fakeFindmnt(t, tc.findmnt)
			env, rec := sudoRecord(t, dir)
			if err := CheckWritable(context.Background(), env, rec); !tc.check(err) {
				t.Fatalf("CheckWritable: %v", err)
			}
		})
	}
}
