package loadgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

// seat is a heavy task's place in the settle and load queues while its
// admission waits for the machine. The task keeps its tickets and holds the
// locks only while it reads the machine and decides: it gives them up for
// each pause, so a priority ticket that joins meanwhile heads the queue and
// takes the lock first, and it never holds the settle lock while it waits in
// the load queue. Before its next reading the task takes the load lock back
// with the same ticket, and the settle lock without waiting beside it. A
// crash drops the tickets and the locks with the process, as it drops any of
// them.
type seat struct {
	holder       string
	priority     bool
	quiet        func() (bool, error)
	log          io.Writer
	settlePath   string
	settleTicket *lock.Ticket
	settleLock   *lock.Lock
	loadPath     string
	loadTicket   *lock.Ticket
	loadLock     *lock.Lock
	// loadPlace is the load ticket the seat left to wait for its settle
	// turn: it rejoins the load queue at that place.
	loadPlace *lock.Ticket
}

// held reports whether the seat holds every lock it queues for.
func (s *seat) held() bool {
	return s.loadLock != nil && (s.settleTicket == nil || s.settleLock != nil)
}

// yield gives the load lock up, then the settle lock, keeping both tickets.
func (s *seat) yield() error {
	var err error
	if s.loadLock != nil {
		err = s.loadLock.Release()
		s.loadLock = nil
	}
	if s.settleLock != nil {
		err = errors.Join(err, s.settleLock.Release())
		s.settleLock = nil
	}
	if err != nil {
		return fmt.Errorf("giving the load queue up for the wait: %w", err)
	}
	return nil
}

// resumed is what taking the locks back waited for.
type resumed struct {
	waited bool
	behind string
	quiet  time.Duration
}

func (r *resumed) note(w lock.FIFOWait) {
	if r.behind == "" {
		r.behind = w.Behind
	}
	r.quiet += w.Quiet
}

// resume takes the load lock by the seat's ticket, then the settle lock
// beside it without waiting. When another settle task goes first or is in
// its settling period, the seat gives the load lock up and leaves the load
// queue, waits for its settle turn holding nothing, so the tasks without a
// settling period do not wait for that one, and rejoins the load queue at
// the place it left, ahead of the tasks that joined after it.
func (s *seat) resume(ctx context.Context) (resumed, error) {
	var r resumed
	for {
		if err := s.takeLoad(ctx, &r); err != nil {
			return r, err
		}
		if s.settleTicket == nil || s.settleLock != nil {
			return r, nil
		}
		lk, ok, err := s.settleTicket.TryAcquire(ctx)
		if err != nil {
			return r, err
		}
		if ok {
			s.settleLock = lk
			return r, nil
		}
		r.waited = true
		if err := s.leaveLoad(); err != nil {
			return r, err
		}
		if err := s.awaitSettleTurn(ctx, &r); err != nil {
			return r, err
		}
	}
}

// resumeAtOnce takes the locks back only when neither would wait: the load
// lock by the seat's ticket, rejoining the load queue at its place first when
// the seat left it, and the settle lock beside it. False, with whatever it
// took given up again and the tickets kept, means a lock would wait; an
// error gives the locks up too, and names the lock whose queue failed.
func (s *seat) resumeAtOnce(ctx context.Context) (bool, string, error) {
	if s.loadLock == nil {
		ticket, err := s.rejoinLoad(ctx)
		if err != nil {
			return false, s.loadPath, errors.Join(err, s.yield())
		}
		lk, ok, err := ticket.TryAcquire(ctx)
		if err != nil || !ok {
			return false, s.loadPath, errors.Join(err, s.yield())
		}
		s.loadLock = lk
	}
	if s.settleTicket != nil && s.settleLock == nil {
		lk, ok, err := s.settleTicket.TryAcquire(ctx)
		if err != nil || !ok {
			return false, s.settlePath, errors.Join(err, s.yield())
		}
		s.settleLock = lk
	}
	return true, "", nil
}

// rejoinLoad returns the seat's ticket in the load queue, put back first
// when the seat left the queue: at its place, or as a new ticket when it had
// none.
func (s *seat) rejoinLoad(ctx context.Context) (*lock.Ticket, error) {
	if s.loadTicket != nil {
		return s.loadTicket, nil
	}
	var t *lock.Ticket
	var err error
	if s.loadPlace != nil {
		t, err = s.loadPlace.Rejoin(ctx)
	} else {
		t, err = lock.JoinFIFO(ctx, s.loadPath, s.holder, s.priority)
	}
	if err != nil {
		return nil, err
	}
	s.loadTicket, s.loadPlace = t, nil
	return t, nil
}

// takeLoad takes the load lock by the seat's ticket, rejoining the load
// queue at its place first when the seat left it.
func (s *seat) takeLoad(ctx context.Context, r *resumed) error {
	if s.loadLock != nil {
		return nil
	}
	ticket, err := s.rejoinLoad(ctx)
	if err != nil {
		return err
	}
	lk, ok, err := ticket.TryAcquire(ctx)
	if err != nil {
		return err
	}
	if !ok {
		r.waited = true
		var w lock.FIFOWait
		lk, w, err = ticket.Acquire(ctx, queueTimeout, s.log, s.quiet)
		r.note(w)
		if err != nil {
			return err
		}
	}
	s.loadLock = lk
	return nil
}

// leaveLoad gives the load lock up and leaves the load queue, keeping the
// place for its return.
func (s *seat) leaveLoad() error {
	var err error
	if s.loadLock != nil {
		err = s.loadLock.Release()
		s.loadLock = nil
	}
	if s.loadTicket != nil {
		err = errors.Join(err, s.loadTicket.Leave())
		s.loadPlace, s.loadTicket = s.loadTicket, nil
	}
	return err
}

// awaitSettleTurn waits until the settle ticket heads its queue and no
// settling period holds the lock, and gives the lock up again: the seat takes
// it only beside the load lock.
func (s *seat) awaitSettleTurn(ctx context.Context, r *resumed) error {
	lk, w, err := s.settleTicket.Acquire(ctx, queueTimeout, s.log, nil)
	r.note(w)
	if err != nil {
		return err
	}
	return lk.Release()
}

// admitHere is the admission's boundary: with the locks held, it checks
// under each queue's guard, the settle queue's taken first and kept, that the
// seat's tickets head their queues, and leaves both queues in that one
// decision. False, with the tickets kept, means a priority ticket joined
// ahead: it goes first. A ticket that joins after this decision finds the
// reserve written when it takes the lock, and a settle ticket the settling
// period it waits for.
func (s *seat) admitHere(ctx context.Context) (bool, error) {
	if s.loadTicket == nil {
		return false, errors.New("the admission has no ticket in the load queue")
	}
	if s.settleTicket == nil {
		left, err := s.loadTicket.LeaveIfHead(ctx)
		if err != nil || !left {
			return false, err
		}
		s.loadTicket = nil
		return true, nil
	}
	loadLeft := false
	left, err := s.settleTicket.LeaveIfHeadAnd(ctx, func() (bool, error) {
		ok, err := s.loadTicket.LeaveIfHead(ctx)
		loadLeft = ok
		return ok, err
	})
	if loadLeft {
		s.loadTicket = nil
	}
	if err != nil || !left {
		return false, err
	}
	s.settleTicket = nil
	return true, nil
}

// close gives up whatever the seat still holds: its locks and its tickets.
func (s *seat) close() error {
	err := s.yield()
	if s.loadTicket != nil {
		err = errors.Join(err, s.loadTicket.Leave())
		s.loadTicket = nil
	}
	if s.settleTicket != nil {
		err = errors.Join(err, s.settleTicket.Leave())
		s.settleTicket = nil
	}
	return err
}

// noteSeatAhead names what the seat's locks would wait for, a holder or a
// ticket ahead, the load lock's first, as what the task waited behind.
func (m *machine) noteSeatAhead(ctx context.Context) error {
	for _, q := range []struct {
		path   string
		ticket *lock.Ticket
	}{{m.seat.loadPath, m.seat.loadTicket}, {m.seat.settlePath, m.seat.settleTicket}} {
		if q.ticket == nil {
			continue
		}
		if m.noteQueueHolder(q.path) {
			return nil
		}
		ahead, err := q.ticket.AheadOf(ctx)
		if err != nil {
			return err
		}
		if len(ahead) > 0 {
			m.waitedBehind = ahead[0]
			m.lastWaitText = "queued behind " + ahead[0]
			return nil
		}
	}
	return nil
}

// resumeSpent is resumeSeat for a priority task whose budget is spent: it
// takes the locks back only when neither would wait, and the interval from
// mark goes on for the next reading to decide; a lock that would wait
// refuses the task, naming what it would wait for.
func (m *machine) resumeSpent(ctx context.Context, p *Priority, mark, from time.Time, why string) error {
	onceCtx, cancel := context.WithTimeout(ctx, guardWait)
	defer cancel()
	taken, path, err := m.seat.resumeAtOnce(onceCtx)
	if err == nil && taken {
		return nil
	}
	// The wait ends with the budget: the pause goes to why.
	if why != "" {
		m.addWait(why, mark, from)
	}
	if err != nil {
		return m.guardFailure(ctx, p, path, err)
	}
	var noteErr error
	if m.lastWaitText == "" {
		noteErr = m.noteSeatAhead(onceCtx)
	}
	if errors.Is(noteErr, context.DeadlineExceeded) {
		// The refusal stands without the name.
		noteErr = nil
	}
	return errors.Join(m.priorityExhausted(p), noteErr)
}

// resumeSeat takes the seat's locks back after a pause or a priority ticket
// that went first. The time taking them waited goes to the queue, or to quiet
// mode while it held the queue, and the wait before it, from mark, to why; a
// resume that did not wait leaves the interval from mark open.
func (m *machine) resumeSeat(ctx context.Context, p *Priority, mark *time.Time, why string) error {
	if m.seat == nil || m.seat.held() {
		return nil
	}
	from := m.now()
	resumeCtx := ctx
	if p != nil {
		// The pause before is spent first: the budget left bounds the wait
		// for the locks.
		left, err := p.spend(*mark, from)
		if err != nil {
			if why != "" {
				m.addWait(why, *mark, from)
			}
			return err
		}
		if left <= 0 {
			return m.resumeSpent(ctx, p, *mark, from, why)
		}
		var cancel context.CancelFunc
		resumeCtx, cancel = context.WithTimeout(ctx, left)
		defer cancel()
	}
	r, err := m.seat.resume(resumeCtx)
	now := m.now()
	if !r.waited && err == nil {
		// Taken back at once: the interval from mark goes on.
		return nil
	}
	if !r.waited {
		// The wait ended before the locks were asked for again: the
		// interval from mark ends with it.
		if why != "" {
			m.addWait(why, *mark, now)
		}
		return err
	}
	if why != "" {
		m.addWait(why, *mark, from)
	}
	quiet := min(r.quiet, now.Sub(from))
	m.addWait(WhyQueue, from, now.Add(-quiet))
	m.addWait(WhyQuiet, now.Add(-quiet), now)
	*mark = now
	if r.behind != "" {
		m.lastWaitText = "queued behind " + r.behind
		if m.waitedBehind == "" {
			m.waitedBehind = r.behind
		}
	}
	if err != nil {
		if p != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return m.priorityFailure(ctx, p, from, now)
		}
		return err
	}
	return nil
}
