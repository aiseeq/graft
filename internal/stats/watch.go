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
	// LoadStart and LoadMax are the 1-minute load average when the step
	// started and the highest seen while it ran; LoadErr tells why they are
	// unknown.
	LoadStart, LoadMax float64
	LoadErr            error
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

// sample reads the tree and the load every watchInterval until stop is
// closed; a source that fails once is not read again.
func sample(pid int, stop <-chan struct{}) Sample {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	var s Sample
	for first := true; ; first = false {
		if s.TreeErr == nil {
			sum, err := treeMemory(pid)
			s.TreeMem, s.TreeErr = max(s.TreeMem, sum), err
		}
		if s.LoadErr == nil {
			load, err := loadavg.Read()
			if first {
				s.LoadStart = load
			}
			s.LoadMax, s.LoadErr = max(s.LoadMax, load), err
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
