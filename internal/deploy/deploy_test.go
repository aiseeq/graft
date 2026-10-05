package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// What pam_fprintd and sudo print without a terminal when no finger comes.
const (
	timedOut  = "Place your right index finger on the fingerprint reader\nVerification timed out\n"
	noMatch   = "Place your right index finger on the fingerprint reader\nFailed to match fingerprint\n"
	noReader  = ""
	confirmTo = "prod"
)

type step struct {
	ok  bool
	out string
	err error
}

func scripted(steps ...step) (func(context.Context) (bool, []byte, error), *int) {
	calls := 0
	return func(context.Context) (bool, []byte, error) {
		s := steps[calls]
		calls++
		return s.ok, []byte(s.out), s.err
	}, &calls
}

func TestConfirmByFingerprintAsksAgainWhileTheReaderRefuses(t *testing.T) {
	attempt, calls := scripted(step{out: timedOut}, step{out: noMatch}, step{ok: true})
	var log bytes.Buffer
	if err := confirmByFingerprint(context.Background(), confirmTo, time.Minute, &log, attempt); err != nil {
		t.Fatalf("confirmByFingerprint: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("attempts = %d, want 3", *calls)
	}
	for _, want := range []string{"fingerprint attempt 1 refused", "fingerprint attempt 2 refused", "Ctrl-C to stop"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q lacks %q", log.String(), want)
		}
	}
}

func TestConfirmByFingerprintStopsWhenTheReaderWasNotAsked(t *testing.T) {
	attempt, calls := scripted(step{out: noReader})
	err := confirmByFingerprint(context.Background(), confirmTo, time.Minute, &bytes.Buffer{}, attempt)
	if err == nil || !strings.Contains(err.Error(), "the fingerprint reader did not answer") {
		t.Fatalf("err = %v, want the reader-did-not-answer refusal", err)
	}
	if *calls != 1 {
		t.Fatalf("attempts = %d, want 1", *calls)
	}
}

func TestConfirmByFingerprintReturnsAFailedRun(t *testing.T) {
	runErr := errors.New("sudo: executable file not found")
	attempt, _ := scripted(step{out: timedOut, err: runErr})
	if err := confirmByFingerprint(context.Background(), confirmTo, time.Minute, &bytes.Buffer{}, attempt); !errors.Is(err, runErr) {
		t.Fatalf("err = %v, want %v", err, runErr)
	}
}

func TestConfirmByFingerprintGivesUpAfterTheWait(t *testing.T) {
	// A sudo still waiting on the reader is killed when the wait runs out.
	attempt := func(ctx context.Context) (bool, []byte, error) {
		<-ctx.Done()
		return false, []byte("Place your right index finger on the fingerprint reader\n"), nil
	}
	err := confirmByFingerprint(context.Background(), confirmTo, 20*time.Millisecond, &bytes.Buffer{}, attempt)
	if err == nil || !strings.Contains(err.Error(), "no fingerprint within 20ms") {
		t.Fatalf("err = %v, want the wait limit", err)
	}
}

func TestConfirmByFingerprintStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempt := func(context.Context) (bool, []byte, error) {
		cancel()
		return false, []byte(timedOut), nil
	}
	if err := confirmByFingerprint(ctx, confirmTo, time.Minute, &bytes.Buffer{}, attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
