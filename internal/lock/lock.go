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
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
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
	fl       *flock.Flock
	noteLock *flock.Flock
	note     string
	// Waited is how long taking it waited for other holders; 0 when it
	// was free.
	Waited       time.Duration
	WaitedBehind string
	WaitedQuiet  time.Duration
}

// Acquire takes the exclusive lock at path. See AcquireMode.
func Acquire(ctx context.Context, path string, timeout time.Duration, holder string, log io.Writer) (*Lock, error) {
	return AcquireMode(ctx, path, Exclusive, timeout, holder, log)
}

// TryAcquire takes the exclusive lock without waiting. A nil lock and nil
// error means another process holds it.
func TryAcquire(path, holder string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if !ok {
		return nil, nil
	}
	noteLock, note, err := createLive(filepath.Dir(path), filepath.Base(path)+".*.lholder", fmt.Appendf(nil, "%s (write), pid %d, since %s", holder, os.Getpid(), time.Now().Format(time.RFC3339)))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("writing lock holder note: %w", err), fl.Unlock())
	}
	return &Lock{fl: fl, noteLock: noteLock, note: note}, nil
}

// acquireFIFOObserved queues a ticket for the lock at path, waits until it
// heads the queue and the lock is free, takes the lock and leaves the queue:
// one hold of a Ticket. quiet classifies the intervals it waits behind the
// head (true when the head's blocker is quiet mode). Tickets are shared by
// processes through live files next to the lock, and the tickets no process
// holds are removed while inspecting the queue.
func acquireFIFOObserved(ctx context.Context, path string, timeout time.Duration, holder string, log io.Writer, quiet func() (bool, error), priority bool) (lk *Lock, retErr error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticket, err := JoinFIFO(waitCtx, path, holder, priority)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := ticket.Leave(); err != nil {
			retErr = errors.Join(retErr, err)
			if lk != nil {
				retErr = errors.Join(retErr, lk.Release())
				lk = nil
			}
		}
	}()
	lk, _, err = waitFIFO(waitCtx, ctx, path, timeout, ticket.name, holder, log, quiet)
	return lk, err
}

// FIFOWait is what a wait in a FIFO queue saw, kept also when the wait
// fails: whom it waited behind and how long while quiet mode was on.
type FIFOWait struct {
	Behind string
	Quiet  time.Duration
}

func waitFIFO(waitCtx, ctx context.Context, path string, timeout time.Duration, ticket, holder string, log io.Writer, quiet func() (bool, error)) (*Lock, FIFOWait, error) {
	var said string
	var behind string
	var waitedOn string
	var lockWaitStart time.Time
	var waitedQuiet time.Duration
	seen := func() FIFOWait {
		if behind != "" {
			return FIFOWait{Behind: behind, Quiet: waitedQuiet}
		}
		return FIFOWait{Behind: waitedOn, Quiet: waitedQuiet}
	}
	for {
		queue, err := queueForTicket(waitCtx, path, ticket)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, seen(), fmt.Errorf("lock %s still queued after %s (%s)", filepath.Base(path), timeout, Describe(path))
		}
		if err != nil {
			return nil, seen(), err
		}
		if queue[0].path == ticket {
			lk, acquired, err := tryAcquireFIFOHead(waitCtx, path, ticket, holder)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					return nil, seen(), fmt.Errorf("lock %s still queued after %s (%s)", filepath.Base(path), timeout, Describe(path))
				}
				return nil, seen(), err
			}
			if acquired {
				if !lockWaitStart.IsZero() {
					lk.Waited = time.Since(lockWaitStart)
				}
				w := seen()
				lk.WaitedQuiet, lk.WaitedBehind = w.Quiet, w.Behind
				return lk, w, nil
			}
			if waitedOn == "" {
				lockWaitStart = time.Now()
				waitedOn = Describe(path)
				fmt.Fprintf(log, "graft: waiting for the %s lock %s (%s), up to %s\n", Exclusive, filepath.Base(path), waitedOn, timeout)
			}
		}
		said, behind = logQueueAhead(queue, ticket, filepath.Base(path), said, behind, log)
		quietWait, err := waitRetryObserved(waitCtx, quiet)
		waitedQuiet += quietWait
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, seen(), fmt.Errorf("lock %s still queued after %s (%s)", filepath.Base(path), timeout, Describe(path))
			}
			return nil, seen(), err
		}
	}
}

func logQueueAhead(queue []queueEntry, ticket, lockName, said, behind string, log io.Writer) (string, string) {
	var ahead []string
	for _, entry := range queue {
		if entry.path == ticket {
			break
		}
		ahead = append(ahead, entry.holder)
	}
	if len(ahead) == 0 {
		return said, behind
	}
	message := strings.Join(ahead, "; ")
	if behind == "" {
		behind = ahead[0]
	}
	if message != said {
		fmt.Fprintf(log, "graft: waiting in the %s queue behind %s\n", lockName, message)
		said = message
	}
	return said, behind
}

// tryAcquireFIFOHead checks the queue and lock together under the queue guard.
// A busy lock leaves the ticket queued so a later priority ticket can move
// ahead before any waiter acquires the lock.
func tryAcquireFIFOHead(ctx context.Context, path, ticket, holder string) (*Lock, bool, error) {
	guard, err := queueGuard(ctx, path)
	if err != nil {
		return nil, false, fmt.Errorf("checking the queue for %s: %w", path, err)
	}
	queue, readErr := readQueue(path)
	if readErr != nil {
		return nil, false, errors.Join(readErr, guard.Close())
	}
	if len(queue) == 0 || queue[0].path != ticket {
		return nil, false, guard.Close()
	}
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("locking %s: %w", path, err), guard.Close())
	}
	if !ok {
		return nil, false, guard.Close()
	}
	noteLock, note, err := createLive(filepath.Dir(path), filepath.Base(path)+".*.lholder", fmt.Appendf(nil, "%s (write), pid %d, since %s", holder, os.Getpid(), time.Now().Format(time.RFC3339)))
	if err != nil {
		return nil, false, errors.Join(fmt.Errorf("writing lock holder note: %w", err), fl.Unlock(), guard.Close())
	}
	if err := guard.Close(); err != nil {
		return nil, false, errors.Join(fmt.Errorf("closing the queue guard for %s: %w", path, err), fl.Unlock(), releaseLive(noteLock, note))
	}
	return &Lock{fl: fl, noteLock: noteLock, note: note}, true, nil
}

func waitRetryObserved(ctx context.Context, quiet func() (bool, error)) (time.Duration, error) {
	start := time.Now()
	quietAtStart := false
	if quiet != nil {
		var err error
		quietAtStart, err = quiet()
		if err != nil {
			return 0, err
		}
	}
	err := sleepRetry(ctx)
	if quietAtStart {
		return time.Since(start), err
	}
	return 0, err
}

func queueForTicket(ctx context.Context, path, ticket string) ([]queueEntry, error) {
	guard, err := queueGuard(ctx, path)
	if err != nil {
		return nil, err
	}
	queue, readErr := readQueue(path)
	if err := errors.Join(readErr, guard.Close()); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(queue, func(e queueEntry) bool { return e.path == ticket }) {
		return nil, fmt.Errorf("ticket for lock %s disappeared: %s", filepath.Base(path), ticket)
	}
	return queue, nil
}

type queueEntry struct {
	path   string
	holder string
}

// guardRetry bounds the pause between two tries of a busy queue guard. The
// pause is short and random: a guard is held for a moment only, and a waiter
// that tried again at the fixed period of another one's polling could keep
// finding the guard held by it.
const guardRetry = 20 * time.Millisecond

func queueGuard(ctx context.Context, path string) (*flock.Flock, error) {
	guard := flock.New(path + ".queue.lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("locking the queue for %s: %w", path, err)
		}
		ok, err := guard.TryLock()
		if err != nil {
			return nil, fmt.Errorf("locking the queue for %s: %w", path, err)
		}
		if ok {
			return guard, nil
		}
		pause := time.NewTimer(time.Millisecond + rand.N(guardRetry))
		select {
		case <-ctx.Done():
			pause.Stop()
		case <-pause.C:
		}
	}
}

// enqueue writes this process's ticket, holding it live until AcquireFIFO
// is done. Tickets are .lticket files: graft before them judged a ticket by
// its pid, which from another PID namespace finds every ticket dead.
func enqueue(ctx context.Context, path, holder string, priority bool) (*flock.Flock, string, error) {
	fl, ticket, _, err := placeTicket(ctx, path, ticketText(holder), "", priority)
	return fl, ticket, err
}

// ticketText is what a ticket says of its holder: who, its pid and since
// when it queued, as a lock holder's note says it of the lock.
func ticketText(holder string) []byte {
	return fmt.Appendf(nil, "%s, pid %d, queued since %s", holder, os.Getpid(), time.Now().Format(time.RFC3339))
}

// placeTicket writes a live ticket under the queue guard at place, the
// file pattern of a ticket that left the queue, or, when place is empty, at
// the next sequence number of the queue. It returns the place it used.
func placeTicket(ctx context.Context, path string, text []byte, place string, priority bool) (ticketLock *flock.Flock, ticket, used string, retErr error) {
	guard, err := queueGuard(ctx, path)
	if err != nil {
		return nil, "", "", fmt.Errorf("joining the queue for %s: %w", path, err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			if ticket != "" {
				err = errors.Join(err, releaseLive(ticketLock, ticket))
				ticketLock, ticket = nil, ""
			}
			retErr = errors.Join(retErr, fmt.Errorf("closing the queue guard for %s: %w", path, err))
		}
	}()
	if place == "" {
		place, err = nextPlace(path, priority)
		if err != nil {
			return nil, "", "", err
		}
	}
	ticketLock, ticket, err = createLive(filepath.Dir(path), place, text)
	if err != nil {
		return nil, "", "", fmt.Errorf("writing a queue ticket for %s: %w", path, err)
	}
	return ticketLock, ticket, place, nil
}

// nextPlace takes the queue's next sequence number, under the queue guard,
// and returns the file pattern of a ticket with it.
func nextPlace(path string, priority bool) (string, error) {
	sequencePath := path + ".queue.next"
	var sequence uint64
	data, err := os.ReadFile(sequencePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("reading the queue sequence for %s: %w", path, err)
	}
	if len(data) != 0 {
		sequence, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return "", fmt.Errorf("reading the queue sequence for %s: %w", path, err)
		}
	}
	if sequence == ^uint64(0) {
		return "", fmt.Errorf("queue sequence exhausted for %s", path)
	}
	sequence++
	if err := writeQueueFile(sequencePath, []byte(strconv.FormatUint(sequence, 10))); err != nil {
		return "", fmt.Errorf("writing the queue sequence for %s: %w", path, err)
	}
	prefix := ""
	if priority {
		prefix = "-"
	}
	return fmt.Sprintf("%s.queue.%s%020d.*.lticket", filepath.Base(path), prefix, sequence), nil
}

func writeQueueFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("creating a temporary queue file: %w", err)
	}
	temporary := f.Name()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	if err := os.Rename(temporary, path); err != nil {
		return errors.Join(err, os.Remove(temporary))
	}
	return nil
}

func readQueue(path string) ([]queueEntry, error) {
	matches, err := filepath.Glob(path + ".queue.*.lticket")
	if err != nil {
		return nil, fmt.Errorf("listing the queue for %s: %w", path, err)
	}
	entries := make([]queueEntry, 0, len(matches))
	for _, ticket := range matches {
		dead, err := gone(ticket)
		if err != nil {
			return nil, fmt.Errorf("a queue ticket: %w", err)
		}
		if dead {
			continue
		}
		data, err := os.ReadFile(ticket)
		if errors.Is(err, os.ErrNotExist) || busy(err) {
			// Gone, or a reader holds it to remove it as dead.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading a queue ticket: %w", err)
		}
		entries = append(entries, queueEntry{path: ticket, holder: strings.TrimSpace(string(data))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

func sleepRetry(ctx context.Context) error {
	t := time.NewTimer(retryDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// AcquireMode takes the lock at path in mode, waiting up to timeout for
// incompatible holders. Waiting stops early when ctx is cancelled. holder
// describes this process and is shown to anyone who has to wait.
func AcquireMode(ctx context.Context, path string, mode Mode, timeout time.Duration, holder string, log io.Writer) (*Lock, error) {
	return AcquireObserved(ctx, path, mode, timeout, holder, log, nil)
}

// AcquireObserved takes the lock and counts time spent waiting while quiet
// mode is active. quiet is checked once at the start of each retry interval.
func AcquireObserved(ctx context.Context, path string, mode Mode, timeout time.Duration, holder string, log io.Writer, quiet func() (bool, error)) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	fl := flock.New(path)
	try := fl.TryLock
	if mode == Shared {
		try = fl.TryRLock
	}
	ok, err := try()
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	var waited time.Duration
	var waitedBehind string
	var waitedQuiet time.Duration
	if !ok {
		waitedBehind = Describe(path)
		start := time.Now()
		fmt.Fprintf(log, "graft: waiting for the %s lock %s (%s), up to %s\n", mode, filepath.Base(path), Describe(path), timeout)
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		for {
			ok, err = try()
			if err != nil {
				return nil, fmt.Errorf("locking %s: %w", path, err)
			}
			if ok {
				break
			}
			intervalStart := time.Now()
			quietAtStart := false
			if quiet != nil {
				quietAtStart, err = quiet()
				if err != nil {
					return nil, err
				}
			}
			err = sleepRetry(waitCtx)
			if quietAtStart {
				waitedQuiet += time.Since(intervalStart)
			}
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, fmt.Errorf("lock %s still held after %s (%s)", filepath.Base(path), timeout, Describe(path))
			}
			if err != nil {
				return nil, fmt.Errorf("locking %s: %w", path, err)
			}
		}
		waited = time.Since(start)
	}
	// Holder notes live next to the lock, one per holder: on Windows the
	// locked file itself cannot be read by the processes waiting for it.
	// They are .lholder live files (createLive), not .holder ones named by a
	// pid, which sandboxes in their own PID namespace share.
	text := fmt.Sprintf("%s (%s), pid %d, since %s", holder, mode, os.Getpid(), time.Now().Format(time.RFC3339))
	noteLock, note, err := createLive(filepath.Dir(path), filepath.Base(path)+".*.lholder", []byte(text))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("writing lock holder note: %w", err), fl.Unlock())
	}
	return &Lock{fl: fl, noteLock: noteLock, note: note, Waited: waited, WaitedBehind: waitedBehind, WaitedQuiet: waitedQuiet}, nil
}

// Release gives the lock up.
func (l *Lock) Release() error {
	return errors.Join(l.fl.Unlock(), releaseLive(l.noteLock, l.note))
}

// Holders returns the notes of the processes holding the lock at path,
// deleting notes no process holds.
func Holders(path string) ([]string, error) {
	matches, err := filepath.Glob(path + ".*.lholder")
	if err != nil {
		return nil, fmt.Errorf("listing lock holders: %w", err)
	}
	sort.Strings(matches)
	holders := []string{}
	for _, m := range matches {
		dead, err := gone(m)
		if err != nil {
			return nil, fmt.Errorf("a lock holder: %w", err)
		}
		if dead {
			continue
		}
		data, err := os.ReadFile(m)
		if errors.Is(err, os.ErrNotExist) || busy(err) {
			continue // released meanwhile, or being removed as dead
		}
		if err != nil {
			return nil, fmt.Errorf("reading a lock holder: %w", err)
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
