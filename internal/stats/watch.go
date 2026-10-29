package stats

import (
	"errors"
	"fmt"
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
	// mem is the summed memory and writes the summed bytes written so far;
	// ioErr tells why writes is unknown.
	mem, writes int64
	ioErr       error
}

// peakWindow is the shortest stretch a write rate is taken over: one
// 200 ms flush is not a rate the disk is kept busy at.
const peakWindow = time.Second

// writeRate finds the most a process tree wrote in a second from the
// running sums of its writes.
type writeRate struct {
	// last is the latest sum read and total what the tree wrote since the
	// first read; a sum that falls, when a process leaves the tree, adds
	// nothing.
	last, total int64
	points      []writePoint
	peak        int64
	known       bool
}

type writePoint struct {
	at    time.Time
	total int64
}

// add takes the sum of the tree's writes read at at.
func (r *writeRate) add(at time.Time, sum int64) {
	if len(r.points) > 0 {
		r.total += max(sum-r.last, 0)
	}
	r.last = sum
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
				writes.add(time.Now(), now.writes)
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
