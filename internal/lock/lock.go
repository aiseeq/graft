// Package lock provides the repository locks graft takes: an exclusive lock
// around commits, and named read/write locks that let parallel agents share a
// work tree (many test runs at once, a restart alone).
//
// Every lock is an OS file lock (flock, LockFileEx): taking it is atomic and it
// disappears with the process that held it, so a crash never leaves a stale
// lock behind.
package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// retryDelay is how often a waiting process retries the lock.
const retryDelay = 200 * time.Millisecond

// Mode is how a lock is held.
type Mode int

// Lock modes: any number of shared holders, or one exclusive holder.
const (
	Shared Mode = iota
	Exclusive
)

func (m Mode) String() string {
	if m == Shared {
		return "read"
	}
	return "write"
}

// Lock is a held lock.
type Lock struct {
	fl   *flock.Flock
	note string
}

// Acquire takes the exclusive lock at path. See AcquireMode.
func Acquire(ctx context.Context, path string, timeout time.Duration, holder string, log io.Writer) (*Lock, error) {
	return AcquireMode(ctx, path, Exclusive, timeout, holder, log)
}

// AcquireMode takes the lock at path in mode, waiting up to timeout for
// incompatible holders. Waiting stops early when ctx is cancelled. holder
// describes this process and is shown to anyone who has to wait.
func AcquireMode(ctx context.Context, path string, mode Mode, timeout time.Duration, holder string, log io.Writer) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	fl := flock.New(path)
	try, tryCtx := fl.TryLock, fl.TryLockContext
	if mode == Shared {
		try, tryCtx = fl.TryRLock, fl.TryRLockContext
	}
	ok, err := try()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		fmt.Fprintf(log, "graft: waiting for the %s lock %s (%s), up to %s\n", mode, filepath.Base(path), Describe(path), timeout)
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ok, err = tryCtx(waitCtx, retryDelay)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("lock %s still held after %s (%s)", filepath.Base(path), timeout, Describe(path))
		}
		if err != nil {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if !ok {
			return nil, fmt.Errorf("could not lock %s", path)
		}
	}
	// Holder notes live next to the lock, one per process: on Windows the
	// locked file itself cannot be read by the processes waiting for it.
	note := notePath(path, os.Getpid())
	text := fmt.Sprintf("%s (%s), pid %d, since %s", holder, mode, os.Getpid(), time.Now().Format(time.RFC3339))
	if err := os.WriteFile(note, []byte(text), 0o644); err != nil {
		return nil, errors.Join(fmt.Errorf("writing lock holder note: %w", err), fl.Unlock())
	}
	return &Lock{fl: fl, note: note}, nil
}

// Release gives the lock up.
func (l *Lock) Release() error {
	noteErr := os.Remove(l.note)
	if errors.Is(noteErr, os.ErrNotExist) {
		noteErr = nil
	}
	return errors.Join(l.fl.Unlock(), noteErr)
}

func notePath(path string, pid int) string {
	return path + "." + strconv.Itoa(pid) + ".holder"
}

// Holders returns the notes of the live processes holding the lock at path,
// deleting notes left behind by processes that died.
func Holders(path string) ([]string, error) {
	matches, err := filepath.Glob(path + ".*.holder")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	holders := []string{}
	for _, m := range matches {
		pidText := strings.TrimSuffix(strings.TrimPrefix(m, path+"."), ".holder")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			return nil, fmt.Errorf("unexpected file %s next to the lock: %w", m, err)
		}
		alive, err := processAlive(pid)
		if err != nil {
			return nil, err
		}
		if !alive {
			if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		data, err := os.ReadFile(m)
		if errors.Is(err, os.ErrNotExist) {
			continue // released meanwhile
		}
		if err != nil {
			return nil, err
		}
		holders = append(holders, strings.TrimSpace(string(data)))
	}
	return holders, nil
}

// Describe summarises who holds the lock, for messages.
func Describe(path string) string {
	holders, err := Holders(path)
	switch {
	case err != nil:
		return "holders unknown: " + err.Error()
	case len(holders) == 0:
		return "holder unknown"
	default:
		return "held by " + strings.Join(holders, "; ")
	}
}
