package loadgate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

type queueSignalWriter chan string

func (w queueSignalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "waiting for the") && strings.Contains(string(p), "lock load.lock") {
		select {
		case w <- string(p):
		default:
		}
	}
	return len(p), nil
}

func TestReserveWrittenBeforeLoadLockHandover(t *testing.T) {
	fakeMachine(t)
	path := filepath.Join(t.TempDir(), "load.lock")
	enteredHandover := make(chan struct{})
	allowRelease := make(chan struct{})
	gotQueue := make(chan string, 1)
	observedReserve := make(chan bool, 1)
	reserveAtHandover := make(chan bool, 1)
	var watching atomic.Bool
	var handoverCalled atomic.Bool
	var blockOnce sync.Once
	var releaseOnce sync.Once
	allow := func() { releaseOnce.Do(func() { close(allowRelease) }) }
	type result struct {
		release func() error
		err     error
	}
	oldBeforeRelease, oldAcquired, oldRelease := loadLockBeforeRelease, loadLockAcquired, loadLockRelease
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Second)
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 10*time.Second)
	first := make(chan result, 1)
	second := make(chan result, 1)
	firstStarted, secondStarted := false, false
	loadLockBeforeRelease = func() {
		blockOnce.Do(func() {
			close(enteredHandover)
			<-allowRelease
		})
	}
	loadLockAcquired = func(lockPath string) {
		if !watching.Load() {
			return
		}
		if !handoverCalled.Load() {
			observedReserve <- false
			return
		}
		files, err := filepath.Glob(admittedFilesPattern(lockPath))
		if err != nil || len(files) == 0 {
			observedReserve <- false
			return
		}
		data, err := os.ReadFile(files[0])
		if err != nil {
			observedReserve <- false
			return
		}
		var record reserveData
		err = json.Unmarshal(data, &record)
		observedReserve <- err == nil && record.Task == "first" && record.Cores > 0
	}
	loadLockRelease = func(lk *lock.Lock) error {
		files, err := filepath.Glob(admittedFilesPattern(path))
		if err != nil || len(files) == 0 {
			reserveAtHandover <- false
		} else {
			data, readErr := os.ReadFile(files[0])
			var record reserveData
			readErr = errors.Join(readErr, json.Unmarshal(data, &record))
			reserveAtHandover <- readErr == nil && record.Task == "first" && record.Cores > 0
		}
		handoverCalled.Store(true)
		return lk.Release()
	}
	defer func() {
		allow()
		if firstStarted {
			var r result
			canceledForCleanup := false
			select {
			case r = <-first:
			case <-time.After(3 * time.Second):
				canceledForCleanup = true
				cancelFirst()
				r = <-first
			}
			if r.err != nil && !(canceledForCleanup && errors.Is(r.err, context.Canceled)) {
				t.Errorf("first Wait: %v", r.err)
			}
			if r.release != nil {
				if err := r.release(); err != nil {
					t.Errorf("first release: %v", err)
				}
			}
		}
		if secondStarted {
			cancelSecond()
			r := <-second
			if r.err != nil && !errors.Is(r.err, context.Canceled) {
				t.Errorf("second Wait: %v", r.err)
			}
			if r.release != nil {
				if err := r.release(); err != nil {
					t.Errorf("second release: %v", err)
				}
			}
		}
		cancelFirst()
		cancelSecond()
		loadLockBeforeRelease, loadLockAcquired, loadLockRelease = oldBeforeRelease, oldAcquired, oldRelease
	}()

	firstStarted = true
	go func() {
		_, release, err := Wait(firstCtx, "first", "projectA:first", nil, testLoad, path, io.Discard, "")
		first <- result{release: release, err: err}
	}()
	entered := false
	select {
	case <-enteredHandover:
		entered = true
	case <-time.After(3 * time.Second):
		t.Error("first admission did not reach the lock handover")
	}
	if entered {
		watching.Store(true)
		secondStarted = true
		go func() {
			_, release, err := Wait(secondCtx, "next", "projectA:next", nil, testLoad, path, queueSignalWriter(gotQueue), "")
			second <- result{release: release, err: err}
		}()
		select {
		case queueLog := <-gotQueue:
			if !strings.Contains(queueLog, "lock load.lock") {
				t.Errorf("unexpected queue log: %s", strings.TrimSpace(queueLog))
			}
		case <-time.After(3 * time.Second):
			t.Error("next admission did not queue behind load.lock")
		}
	}
	allow()
	select {
	case hasReserve := <-reserveAtHandover:
		if !hasReserve {
			t.Error("previous reserve was not written when load.lock was handed over")
		}
	case <-time.After(3 * time.Second):
		t.Error("load.lock handover was not observed")
	}
	select {
	case hasReserve := <-observedReserve:
		if !hasReserve {
			t.Error("next admission acquired load.lock before seeing the previous reserve")
		}
	case <-time.After(3 * time.Second):
		t.Error("next admission did not report its reserve at lock acquisition")
	}
}
