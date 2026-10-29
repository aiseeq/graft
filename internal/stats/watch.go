package stats

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/aiseeq/graft/v2/internal/loadavg"
)

// ErrNoTree: only Linux shows a process tree's memory, through /proc.
var ErrNoTree = errors.New("the memory of a process tree is read only on Linux")

// watchInterval is how often a running step is sampled. A process that
// lives shorter is seen at most once or not at all; the long-lived ones that
// make up a peak are.
const watchInterval = 200 * time.Millisecond

// Sample is what a Watch saw while a step ran.
type Sample struct {
	// TreeMem is the peak of the summed memory of the process and its
	// descendants, which a single process's ru_maxrss does not show when
	// several run side by side; TreeErr tells why it is unknown.
	TreeMem int64
	TreeErr error
	// PeakWrite is the most the tree wrote in a second, in bytes, known when
	// PeakKnown: a step shorter than a second has no peak. IOErr tells why
	// the writes stopped being read.
	PeakWrite int64
	PeakKnown bool
	IOErr     error
	// LoadStart and LoadMax are the 1-minute load average when the step
	// started and the highest seen while it ran; LoadErr tells why they are
	// unknown.
	LoadStart, LoadMax float64
	LoadErr            error
	LoadKnown          bool
}

// Watch samples a running step until it is stopped.
type Watch struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	sample   Sample
}

// WatchProcess starts sampling the process pid and the machine.
func WatchProcess(pid int) *Watch {
	w := &Watch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer func() {
			if p := recover(); p != nil {
				err := fmt.Errorf("sampling the step panicked: %v", p)
				w.sample.TreeErr, w.sample.LoadErr = err, err
			}
		}()
		w.sample = sample(pid, w.stop)
	}()
	return w
}

// LoadWatch samples the machine for the whole life of a task, including waits
// before its steps.
type LoadWatch struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	sample   Sample
}

// WatchLoad starts sampling the machine load.
func WatchLoad() *LoadWatch {
	w := &LoadWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer func() {
			if p := recover(); p != nil {
				w.mu.Lock()
				w.sample.LoadErr = fmt.Errorf("sampling the task load panicked: %v", p)
				w.mu.Unlock()
			}
		}()
		t := time.NewTicker(watchInterval)
		defer t.Stop()
		for {
			load, err := loadavg.Read()
			w.mu.Lock()
			if err != nil {
				w.sample.LoadErr = err
			} else {
				w.sample.LoadErr = nil
				if !w.sample.LoadKnown {
					w.sample.LoadStart = load
				}
				w.sample.LoadMax = max(w.sample.LoadMax, load)
				w.sample.LoadKnown = true
			}
			w.mu.Unlock()
			select {
			case <-w.stop:
				return
			case <-t.C:
			}
		}
	}()
	return w
}

// Stop ends task load sampling and returns its highest observed load.
func (w *LoadWatch) Stop() Sample {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.sample
	s.TreeErr = ErrNoTree
	return s
}

// treeNow is what readTree saw of a process tree at one moment.
type treeNow struct {
	// mem is the summed memory; procs holds, when the writes are read, every
	// process of the tree by pid; ioErr tells why they are unknown.
	mem   int64
	procs map[int]treeProc
	ioErr error
}

// treeProc is one process of a tree: its parent and, when graft may read
// its io (not one under sudo), what it has caused to be written so far,
// the writes of the children it reaped included.
type treeProc struct {
	parent   int
	writes   int64
	readable bool
}

// peakWindow is the shortest stretch a write rate is taken over: one
// 200 ms flush is not a rate the disk is kept busy at.
const peakWindow = time.Second

// reapReadings is how many readings a reaped child's writes may show up in
// its parent's growth: the reading that finds the child gone, and the next,
// in case the child was reaped while the tree was read.
const reapReadings = 2

// writeRate finds the most a process tree wrote in a second from what its
// processes have written so far. A parent's writes take in the writes of
// the children it reaps: what a child gone from the tree was counted for
// comes off the growth of its nearest ancestor still there, and the growth
// of an ancestor whose gone child held writes graft could not read is left
// out, both for reapReadings. A child that left the tree alive (a daemon)
// costs its parent's writes over them.
type writeRate struct {
	last map[int]tracked
	// credit is what gone children were counted for, by ancestor, still to
	// come off its growth, each for readings of its own; blind is the
	// readings an ancestor's growth is still left out of.
	credit map[int][]credit
	blind  map[int]int
	// total is what the tree wrote since the first reading that read a
	// process.
	total  int64
	points []writePoint
	peak   int64
	known  bool
}

// tracked is what writeRate keeps of a process between readings.
type tracked struct {
	parent int
	// writes is its write_bytes when last read; readable tells it was read
	// at the last reading, and hidden that some reading could not read it,
	// so that write_bytes holds writes left out.
	writes           int64
	readable, hidden bool
}

// credit is what one gone child was counted for, to come off its ancestor's
// growth over the next left readings.
type credit struct {
	bytes int64
	left  int
}

type writePoint struct {
	at    time.Time
	total int64
}

// add takes the tree's processes read at at.
func (r *writeRate) add(at time.Time, now map[int]treeProc) {
	if r.credit == nil {
		r.credit, r.blind = map[int][]credit{}, map[int]int{}
	}
	r.gone(now)
	next := make(map[int]tracked, len(now))
	read := false
	for _, pid := range slices.Sorted(maps.Keys(now)) {
		p := now[pid]
		prev, seen := r.last[pid]
		t := tracked{parent: p.parent, writes: prev.writes, readable: p.readable, hidden: prev.hidden || !p.readable}
		if p.readable {
			var grow int64
			switch {
			case len(r.points) == 0:
				// The counting starts at the first reading that reads a
				// process.
			case !seen:
				grow = p.writes
			case !prev.readable:
				// Read again: what it wrote unseen stays out.
			default:
				grow = max(p.writes-prev.writes, 0)
			}
			read = true
			grow = r.takeCredit(pid, grow)
			if r.blind[pid] == 0 {
				r.total += grow
			}
			t.writes = p.writes
		}
		next[pid] = t
	}
	for _, pid := range slices.Sorted(maps.Keys(r.blind)) {
		if r.blind[pid]--; r.blind[pid] <= 0 {
			delete(r.blind, pid)
		}
	}
	for _, pid := range slices.Sorted(maps.Keys(r.credit)) {
		left := r.credit[pid][:0]
		for _, c := range r.credit[pid] {
			if c.left--; c.left > 0 && c.bytes > 0 {
				left = append(left, c)
			}
		}
		r.credit[pid] = left
		if len(left) == 0 {
			delete(r.credit, pid)
		}
	}
	r.last = next
	if !read {
		return
	}
	r.points = append(r.points, writePoint{at, r.total})
	from := -1
	for i, p := range r.points {
		if at.Sub(p.at) >= peakWindow {
			from = i
		}
	}
	if from < 0 {
		return
	}
	p := r.points[from]
	rate := int64(float64(r.total-p.total) / at.Sub(p.at).Seconds())
	r.peak, r.known = max(r.peak, rate), true
	r.points = r.points[from:]
}

// gone hands what the processes no longer in the tree were counted for, or
// that their writes are unknown, to their nearest ancestor still there,
// which reaps them or the process that did.
func (r *writeRate) gone(now map[int]treeProc) {
	isGone := func(pid int) bool {
		_, ok := now[pid]
		return !ok
	}
	for _, pid := range slices.Sorted(maps.Keys(r.last)) {
		if !isGone(pid) {
			continue
		}
		p := r.last[pid]
		up := p.parent
		// Bounded by the processes there were: a loop of parents, which
		// the kernel does not make, cannot hold the sampler.
		for range len(r.last) {
			a, ok := r.last[up]
			if !ok || !isGone(up) {
				break
			}
			up = a.parent
		}
		if p.hidden {
			r.blind[up] = reapReadings
		} else if p.writes > 0 {
			r.credit[up] = append(r.credit[up], credit{p.writes, reapReadings})
		}
		r.credit[up] = append(r.credit[up], r.credit[pid]...)
		r.blind[up] = max(r.blind[up], r.blind[pid])
		delete(r.credit, pid)
		delete(r.blind, pid)
	}
}

// takeCredit takes what gone children of pid were counted for off its
// growth, the oldest first, and returns what is left of it.
func (r *writeRate) takeCredit(pid int, grow int64) int64 {
	for i := range r.credit[pid] {
		c := &r.credit[pid][i]
		taken := min(c.bytes, grow)
		c.bytes -= taken
		grow -= taken
	}
	return grow
}

// sample reads the tree and the load every watchInterval until stop is
// closed; a source that fails once is not read again.
func sample(pid int, stop <-chan struct{}) (s Sample) {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	var writes writeRate
	defer func() { s.PeakWrite, s.PeakKnown = writes.peak, writes.known }()
	for first := true; ; first = false {
		if s.TreeErr == nil {
			now, err := readTree(pid, s.IOErr == nil)
			s.TreeMem, s.TreeErr = max(s.TreeMem, now.mem), err
			switch {
			case err != nil, s.IOErr != nil:
			case now.ioErr != nil:
				s.IOErr = now.ioErr
			default:
				writes.add(time.Now(), now.procs)
			}
		}
		if s.LoadErr == nil {
			load, err := loadavg.Read()
			if first {
				s.LoadStart = load
			}
			if err == nil {
				if !s.LoadKnown {
					s.LoadStart = load
				}
				s.LoadMax = max(s.LoadMax, load)
				s.LoadKnown = true
			} else {
				s.LoadErr = err
			}
		}
		if s.TreeErr != nil && s.LoadErr != nil {
			return s
		}
		select {
		case <-stop:
			return s
		case <-t.C:
		}
	}
}

// Stop ends the sampling and returns what it saw.
func (w *Watch) Stop() Sample {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	return w.sample
}

// Unsupported tells whether err only means the system does not offer the
// measure: nothing to warn about.
func Unsupported(err error) bool {
	return errors.Is(err, ErrNoTree) || errors.Is(err, loadavg.ErrNone)
}
