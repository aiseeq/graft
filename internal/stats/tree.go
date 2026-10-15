package stats

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoTree: only Linux shows a process tree's memory, through /proc.
var ErrNoTree = errors.New("the memory of a process tree is read only on Linux")

// treeInterval is how often the memory of a step's process tree is read.
// A process that lives shorter is seen at most once or not at all; the
// long-lived ones that make up a peak are.
const treeInterval = 200 * time.Millisecond

// TreeWatch samples the memory of a process and its descendants until it is
// stopped: the peak of their sum, which a single process's ru_maxrss does
// not show when several run side by side.
type TreeWatch struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	peak     int64
	err      error
}

// WatchTree starts sampling the tree under pid.
func WatchTree(pid int) *TreeWatch {
	w := &TreeWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer func() {
			if p := recover(); p != nil {
				w.err = fmt.Errorf("sampling the process tree panicked: %v", p)
			}
		}()
		w.peak, w.err = sample(pid, w.stop)
	}()
	return w
}

// sample reads the tree every treeInterval until stop is closed or a read
// fails, and returns the largest sum.
func sample(pid int, stop <-chan struct{}) (int64, error) {
	t := time.NewTicker(treeInterval)
	defer t.Stop()
	var peak int64
	for {
		sum, err := treeMemory(pid)
		if err != nil {
			return peak, err
		}
		peak = max(peak, sum)
		select {
		case <-stop:
			return peak, nil
		case <-t.C:
		}
	}
}

// Stop ends the sampling and returns the peak in bytes; an error means the
// tree could not be read and the peak is unknown.
func (w *TreeWatch) Stop() (int64, error) {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	return w.peak, w.err
}
