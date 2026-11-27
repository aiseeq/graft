package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Ticket is a place in the queue of a FIFO lock that outlives one hold of
// the lock: a holder that gives the lock up for a while keeps its place, and
// the priority tickets that joined meanwhile sort ahead of it and take the
// lock first. It is the .lticket file every graft since 2.43 reads, live
// while this process holds it: a crash drops it as it drops any ticket.
type Ticket struct {
	path   string
	holder string
	text   []byte
	place  string
	fl     *flock.Flock
	name   string
	once   sync.Once
	err    error
}

// JoinFIFO puts a ticket for the lock at path into its queue: ahead of the
// ordinary tickets when priority is set, in FIFO order within each kind.
func JoinFIFO(ctx context.Context, path, holder string, priority bool) (*Ticket, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	text := ticketText(holder)
	fl, name, place, err := placeTicket(ctx, path, text, "", priority)
	if err != nil {
		return nil, err
	}
	return &Ticket{path: path, holder: holder, text: text, place: place, fl: fl, name: name}, nil
}

// Rejoin puts a new ticket into the queue at the place of t, which left it:
// ahead of the tickets that joined after t, behind those that joined before
// it and are still queued. A task that steps out of the queue for a while
// takes its turn back with it.
func (t *Ticket) Rejoin(ctx context.Context) (*Ticket, error) {
	fl, name, place, err := placeTicket(ctx, t.path, t.text, t.place, false)
	if err != nil {
		return nil, err
	}
	return &Ticket{path: t.path, holder: t.holder, text: t.text, place: place, fl: fl, name: name}, nil
}

// Acquire waits up to timeout until the ticket heads the queue and the lock
// is free, and takes the lock; the ticket stays queued. quiet, when set, is
// checked at the start of each retry interval: true counts the interval as
// quiet mode. The FIFOWait comes back with an error too.
func (t *Ticket) Acquire(ctx context.Context, timeout time.Duration, log io.Writer, quiet func() (bool, error)) (*Lock, FIFOWait, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return waitFIFO(waitCtx, ctx, t.path, timeout, t.name, t.holder, log, quiet)
}

// TryAcquire takes the lock without waiting when the ticket heads the queue
// and the lock is free; ok is false otherwise. The ticket stays queued.
func (t *Ticket) TryAcquire(ctx context.Context) (lk *Lock, ok bool, err error) {
	return tryAcquireFIFOHead(ctx, t.path, t.name, t.holder)
}

// Head reports whether the ticket heads the queue, read under the queue
// guard.
func (t *Ticket) Head(ctx context.Context) (bool, error) {
	guard, err := queueGuard(ctx, t.path)
	if err != nil {
		return false, fmt.Errorf("checking the queue for %s: %w", t.path, err)
	}
	queue, readErr := readQueue(t.path)
	if err := errors.Join(readErr, guard.Close()); err != nil {
		return false, err
	}
	return headOf(queue, t.name, t.path)
}

// AheadOf returns the holders of the live tickets queued ahead of the
// ticket, head first, read under the queue guard.
func (t *Ticket) AheadOf(ctx context.Context) ([]string, error) {
	guard, err := queueGuard(ctx, t.path)
	if err != nil {
		return nil, fmt.Errorf("checking the queue for %s: %w", t.path, err)
	}
	queue, readErr := readQueue(t.path)
	if err := errors.Join(readErr, guard.Close()); err != nil {
		return nil, err
	}
	var ahead []string
	for _, entry := range queue {
		if entry.path == t.name {
			return ahead, nil
		}
		ahead = append(ahead, entry.holder)
	}
	return nil, fmt.Errorf("ticket for lock %s disappeared: %s", filepath.Base(t.path), t.name)
}

// LeaveIfHead removes the ticket from the queue when it heads it, under the
// queue guard; left is false, and the ticket stays, when another ticket
// heads the queue. A ticket that joins the queue takes the same guard, so it
// joined either before this decision and heads the queue here, or after it
// and finds this ticket gone.
func (t *Ticket) LeaveIfHead(ctx context.Context) (left bool, err error) {
	return t.LeaveIfHeadAnd(ctx, nil)
}

// LeaveIfHeadAnd is LeaveIfHead that also asks also, under this queue's
// guard and only when the ticket heads the queue: the ticket leaves only
// when also returns true. It joins the decisions of two queues into one, the
// guard of this queue taken first.
func (t *Ticket) LeaveIfHeadAnd(ctx context.Context, also func() (bool, error)) (left bool, err error) {
	guard, err := queueGuard(ctx, t.path)
	if err != nil {
		return false, fmt.Errorf("checking the queue for %s: %w", t.path, err)
	}
	queue, readErr := readQueue(t.path)
	if readErr != nil {
		return false, errors.Join(readErr, guard.Close())
	}
	head, err := headOf(queue, t.name, t.path)
	if err != nil || !head {
		return false, errors.Join(err, guard.Close())
	}
	if also != nil {
		ok, err := also()
		if err != nil || !ok {
			return false, errors.Join(err, guard.Close())
		}
	}
	return true, errors.Join(t.Leave(), guard.Close())
}

// Leave removes the ticket from the queue; later calls return the first
// call's error.
func (t *Ticket) Leave() error {
	t.once.Do(func() {
		if err := releaseLive(t.fl, t.name); err != nil {
			t.err = fmt.Errorf("removing queue ticket for %s: %w", t.path, err)
		}
	})
	return t.err
}

func headOf(queue []queueEntry, ticket, path string) (bool, error) {
	if len(queue) > 0 && queue[0].path == ticket {
		return true, nil
	}
	for _, entry := range queue {
		if entry.path == ticket {
			return false, nil
		}
	}
	return false, fmt.Errorf("ticket for lock %s disappeared: %s", filepath.Base(path), ticket)
}

// Queue returns the holders of the live tickets queued for the lock at path,
// head first, dropping the tickets no process holds.
func Queue(ctx context.Context, path string) ([]string, error) {
	return Ahead(ctx, path, false)
}

// Ahead returns the holders of the live tickets, head first, that a ticket
// joining the queue at path now would queue behind: every ticket for an
// ordinary one, the priority tickets for a priority one.
func Ahead(ctx context.Context, path string, priority bool) ([]string, error) {
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	guard, err := queueGuard(ctx, path)
	if err != nil {
		return nil, err
	}
	queue, readErr := readQueue(path)
	if err := errors.Join(readErr, guard.Close()); err != nil {
		return nil, err
	}
	priorityTickets := filepath.Base(path) + ".queue.-"
	holders := make([]string, 0, len(queue))
	for _, entry := range queue {
		if priority && !strings.HasPrefix(filepath.Base(entry.path), priorityTickets) {
			continue
		}
		holders = append(holders, entry.holder)
	}
	return holders, nil
}
