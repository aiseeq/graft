// Package lock serialises graft operations on one repository. Two commits
// running side by side would both bump the version from the same value and
// race on the index.
package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// retryDelay is how often a waiting process retries the lock.
const retryDelay = 200 * time.Millisecond

// Lock is a held repository lock.
type Lock struct {
	fl *flock.Flock
}

// Acquire takes the lock at path, waiting up to timeout for the current holder.
// The lock is an OS file lock (flock, LockFileEx): it disappears with the
// process that held it, so a crash never leaves a stale lock behind. Waiting
// stops early when ctx is cancelled. holder describes this process and is
// shown to anyone who has to wait.
func Acquire(ctx context.Context, path string, timeout time.Duration, holder string, log io.Writer) (*Lock, error) {
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		fmt.Fprintf(log, "graft: waiting for the repository lock (%s), up to %s\n", describeHolder(path), timeout)
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ok, err = fl.TryLockContext(waitCtx, retryDelay)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("repository lock still held after %s (%s)", timeout, describeHolder(path))
		}
		if err != nil {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if !ok {
			return nil, fmt.Errorf("could not lock %s", path)
		}
	}
	// The holder note lives next to the lock: on Windows the locked file
	// itself cannot be read by the processes that wait for it.
	note := fmt.Sprintf("%s, pid %d, since %s", holder, os.Getpid(), time.Now().Format(time.RFC3339))
	if err := os.WriteFile(holderPath(path), []byte(note), 0o644); err != nil {
		return nil, errors.Join(fmt.Errorf("writing lock holder note: %w", err), fl.Unlock())
	}
	return &Lock{fl: fl}, nil
}

// Release gives the lock up.
func (l *Lock) Release() error {
	return l.fl.Unlock()
}

func holderPath(path string) string { return path + ".holder" }

func describeHolder(path string) string {
	data, err := os.ReadFile(holderPath(path))
	if err != nil {
		return "holder unknown: " + err.Error()
	}
	return "held by " + strings.TrimSpace(string(data))
}
