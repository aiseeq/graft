package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

const (
	testTempPrefix    = "graft-it-"
	testTempLockName  = ".graft-it.lock"
	testTempSweepAge  = 24 * time.Hour
	testTempHelperEnv = "GRAFT_TEST_TEMP_HELPER"
	testTempReadyEnv  = "GRAFT_TEST_TEMP_READY"
	testTempPauseEnv  = "GRAFT_TEST_TEMP_PAUSE"
)

func prepareTestTempDir() error {
	testTempMu.Lock()
	defer testTempMu.Unlock()
	root := os.TempDir()
	return withTestTempSweepLock(root, func() error {
		if err := reapStaleTestTempDirs(root, time.Now()); err != nil {
			return err
		}
		dir, err := os.MkdirTemp(root, testTempPrefix)
		if err != nil {
			return fmt.Errorf("a directory for the tests: %w", err)
		}
		lock := flock.New(filepath.Join(dir, testTempLockName))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			_ = os.RemoveAll(dir)
			if err == nil {
				err = fmt.Errorf("new test temp lock was already held")
			}
			return fmt.Errorf("lock the test temp directory: %w", err)
		}
		if pause := os.Getenv(testTempPauseEnv); pause != "" {
			if err := os.WriteFile(pause, []byte(dir), 0o600); err != nil {
				_ = lock.Unlock()
				_ = os.RemoveAll(dir)
				return fmt.Errorf("write test temp pause marker: %w", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(pause + ".continue"); err == nil {
					break
				} else if !os.IsNotExist(err) {
					_ = lock.Unlock()
					_ = os.RemoveAll(dir)
					return fmt.Errorf("read test temp pause marker: %w", err)
				}
				if time.Now().After(deadline) {
					_ = lock.Unlock()
					_ = os.RemoveAll(dir)
					return fmt.Errorf("timed out waiting to resume test temp preparation")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		testTempDir, testTempLock = dir, lock
		return nil
	})
}

func runTempDirTestHelper() bool {
	mode := os.Getenv(testTempHelperEnv)
	if mode == "" {
		return false
	}
	if ready := os.Getenv(testTempReadyEnv); ready != "" {
		if err := os.WriteFile(ready, []byte(currentTestTempDir()), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "write temp helper ready file:", err)
			cleanupTestTempDir()
			os.Exit(2)
		}
	}
	if mode == "wait" {
		select {}
	}
	cleanupTestTempDir()
	os.Exit(0)
	return true
}

func currentTestTempDir() string {
	testTempMu.Lock()
	defer testTempMu.Unlock()
	return testTempDir
}

func cleanupTestTempDir() {
	testTempMu.Lock()
	defer testTempMu.Unlock()
	if testTempDir == "" {
		return
	}
	_ = withTestTempSweepLock(os.TempDir(), func() error {
		if testTempLock != nil {
			_ = testTempLock.Unlock()
			testTempLock = nil
		}
		err := os.RemoveAll(testTempDir)
		testTempDir = ""
		return err
	})
}

func withTestTempSweepLock(root string, fn func() error) error {
	lock := flock.New(filepath.Join(root, ".graft-it-sweep.lock"))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock test temp cleanup: %w", err)
	}
	defer lock.Unlock()
	return fn()
}

// reapStaleTestTempDirs removes abandoned lock-aware dirs immediately. Legacy
// dirs have no lock file, so only remove them after the grace period: an old
// test binary may still be using that format.
func reapStaleTestTempDirs(root string, now time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read temp directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), testTempPrefix) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		lockPath := filepath.Join(path, testTempLockName)
		_, statErr := os.Stat(lockPath)
		if statErr == nil {
			lock := flock.New(lockPath)
			locked, lockErr := lock.TryLock()
			if lockErr != nil {
				continue // Unknown lock state is treated as live.
			}
			if !locked {
				continue
			}
			_ = lock.Unlock()
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove stale test temp directory %s: %w", path, err)
			}
			continue
		}
		if !os.IsNotExist(statErr) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read legacy test temp directory %s: %w", path, err)
		}
		if now.Sub(info.ModTime()) > testTempSweepAge {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove old legacy test temp directory %s: %w", path, err)
			}
		}
	}
	return nil
}

func TestStaleTestTempDirs(t *testing.T) {
	root := t.TempDir()
	dead := filepath.Join(root, "graft-it-dead")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	deadLock := flock.New(filepath.Join(dead, testTempLockName))
	if locked, err := deadLock.TryLock(); err != nil || !locked {
		t.Fatalf("prepare dead run lock: locked=%v err=%v", locked, err)
	}
	if err := deadLock.Unlock(); err != nil {
		t.Fatal(err)
	}

	live := filepath.Join(root, "graft-it-live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	liveLock := flock.New(filepath.Join(live, testTempLockName))
	if locked, err := liveLock.TryLock(); err != nil || !locked {
		t.Fatalf("prepare live run lock: locked=%v err=%v", locked, err)
	}
	defer liveLock.Unlock()

	legacy := filepath.Join(root, "graft-it-legacy")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := reapStaleTestTempDirs(root, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dead} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("dead run still exists (stat err %v)", err)
		}
	}
	for _, path := range []string{live, legacy} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("protected run %s was removed: %v", path, err)
		}
	}
}
