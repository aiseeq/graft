package lock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTicketKeepsItsPlaceWithoutTheLock: a ticket whose holder gave the lock
// up keeps the ordinary tickets behind it waiting, a priority ticket that
// joins takes the lock ahead of it, and leaving works only from the head.
func TestTicketKeepsItsPlaceWithoutTheLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "load.lock")
	first, err := JoinFIFO(ctx, path, "first", false)
	if err != nil {
		t.Fatal(err)
	}
	lk, _, err := first.Acquire(ctx, time.Second, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	results := make(chan fifoResult, 1)
	go func() {
		lk, err := acquireFIFOObserved(ctx, path, 10*time.Second, "second", io.Discard, nil, false)
		results <- fifoResult{lock: lk, err: err}
	}()
	waitForTickets(t, path, 2)
	select {
	case got := <-results:
		t.Fatalf("an ordinary ticket behind a kept one took the free lock: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}

	priority, err := JoinFIFO(ctx, path, "priority", true)
	if err != nil {
		t.Fatal(err)
	}
	if lk, ok, err := first.TryAcquire(ctx); err != nil || ok {
		if lk != nil {
			_ = lk.Release()
		}
		t.Fatalf("TryAcquire behind a priority ticket = %v, %v; want not taken", ok, err)
	}
	if head, err := first.Head(ctx); err != nil || head {
		t.Fatalf("Head behind a priority ticket = %v, %v; want false", head, err)
	}
	if left, err := first.LeaveIfHead(ctx); err != nil || left {
		t.Fatalf("LeaveIfHead behind a priority ticket = %v, %v; want kept", left, err)
	}
	queued, err := Queue(ctx, path)
	if err != nil || len(queued) != 3 || holderName(queued[0]) != "priority" || holderName(queued[1]) != "first" || holderName(queued[2]) != "second" {
		t.Fatalf("queue = %v, %v; want priority, first, second", queued, err)
	}
	priorityLock, _, err := priority.Acquire(ctx, time.Second, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := priority.Leave(); err != nil {
		t.Fatal(err)
	}
	if err := priorityLock.Release(); err != nil {
		t.Fatal(err)
	}

	if left, err := first.LeaveIfHead(ctx); err != nil || !left {
		t.Fatalf("LeaveIfHead at the head = %v, %v; want left", left, err)
	}
	if err := first.Leave(); err != nil {
		t.Fatalf("a second Leave: %v", err)
	}
	got := receiveLock(t, results)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if err := got.lock.Release(); err != nil {
		t.Fatal(err)
	}
	if queued, err := Queue(ctx, path); err != nil || len(queued) != 0 {
		t.Fatalf("queue after all = %v, %v; want empty", queued, err)
	}
}

// holderName is the holder a ticket names, without its pid and queue time.
func holderName(text string) string {
	name, _, _ := strings.Cut(text, ", pid ")
	return name
}

// TestTicketNamesItsHolderPidAndQueueTime: a task queued behind a ticket
// whose holder gave the lock up learns the holder's pid and when it queued,
// as a lock's holder note tells them.
func TestTicketNamesItsHolderPidAndQueueTime(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "load.lock")
	head, err := JoinFIFO(ctx, path, "graft head", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = head.Leave() }()
	next, err := JoinFIFO(ctx, path, "graft next", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Leave() }()
	var log strings.Builder
	_, w, err := next.Acquire(ctx, 100*time.Millisecond, &log, nil)
	if err == nil {
		t.Fatal("the second ticket took the lock behind the head")
	}
	want := fmt.Sprintf("graft head, pid %d, queued since ", os.Getpid())
	if !strings.HasPrefix(w.Behind, want) || !strings.Contains(log.String(), "behind "+want) {
		t.Errorf("waited behind %q, log %q; want %q with the time", w.Behind, log.String(), want)
	}
}

// TestTicketRejoinsAtItsPlace: a ticket that left the queue and rejoins it
// queues ahead of the tickets that joined after it, behind those still
// queued from before it, and a priority ticket keeps its priority.
func TestTicketRejoinsAtItsPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "load.lock")
	join := func(holder string, priority bool) *Ticket {
		t.Helper()
		ticket, err := JoinFIFO(ctx, path, holder, priority)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ticket.Leave() })
		return ticket
	}
	join("before", false)
	left := join("left", false)
	leftPriority := join("left priority", true)
	if err := errors.Join(left.Leave(), leftPriority.Leave()); err != nil {
		t.Fatal(err)
	}
	join("after", false)
	join("after priority", true)
	for _, ticket := range []*Ticket{left, leftPriority} {
		back, err := ticket.Rejoin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = back.Leave() })
	}
	queued, err := Queue(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, holder := range queued {
		got = append(got, holderName(holder))
	}
	if want := []string{"left priority", "after priority", "before", "left", "after"}; !slices.Equal(got, want) {
		t.Errorf("queue = %v; want %v", got, want)
	}
}

// TestQueueGuardComesThroughABusyGuard: a guard that another waiter takes
// nine tenths of the time still comes within a second: the tries are close
// and at random times, not at a period that can keep meeting the other's.
func TestQueueGuardComesThroughABusyGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			guard, err := queueGuard(context.Background(), path)
			if err != nil {
				done <- err
				return
			}
			time.Sleep(9 * time.Millisecond)
			if err := guard.Close(); err != nil {
				done <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		guard, err := queueGuard(ctx, path)
		cancel()
		if err != nil {
			t.Fatalf("the guard did not come within a second of a busy one: %v", err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTicketAheadOfNamesTheTicketsBeforeIt: a ticket names the holders
// queued ahead of it, a priority ticket that joined later first.
func TestTicketAheadOfNamesTheTicketsBeforeIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	ctx := context.Background()
	var tickets []*Ticket
	for _, join := range []struct {
		holder   string
		priority bool
	}{{"projectA first", false}, {"projectA second", false}, {"projectA deploy", true}} {
		ticket, err := JoinFIFO(ctx, path, join.holder, join.priority)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ticket.Leave() }()
		tickets = append(tickets, ticket)
	}
	for i, want := range [][]string{{"projectA deploy"}, {"projectA deploy", "projectA first"}, nil} {
		ahead, err := tickets[i].AheadOf(ctx)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(ahead))
		for _, holder := range ahead {
			names = append(names, holderName(holder))
		}
		if !slices.Equal(names, want) && (len(names) != 0 || len(want) != 0) {
			t.Errorf("ahead of ticket %d = %v; want %v", i, names, want)
		}
	}
	if err := tickets[0].Leave(); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets[0].AheadOf(ctx); err == nil {
		t.Error("a ticket that left found its place in the queue")
	}
}
