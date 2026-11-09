package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
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
	// Taken from the real sudo 1.9.17, LC_ALL=C, stdin not a terminal: the same
	// with the reader held by another process and with no reader.
	notAsked = "sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper\nsudo: a password is required\n"
	// Taken from the real fprintd-list 1.94.5.
	fingerListed = "found 1 devices\nDevice at /net/reactivated/Fprint/Device/0\nUsing device /net/reactivated/Fprint/Device/0\nFingerprints for user someone on Reader (press):\n - #0: right-middle-finger\n"
)

func limitsOf(wait time.Duration) fingerprintLimits {
	return fingerprintLimits{wait: wait, busyWait: time.Minute, now: time.Now}
}

func enrolledOK(context.Context) error { return nil }

// neverAsked fails a test where nothing should ask fprintd.
func neverAsked(t *testing.T) func(context.Context) error {
	return func(context.Context) error {
		t.Error("asked fprintd for enrolled fingers")
		return nil
	}
}

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
	if err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &log, enrolledOK, attempt); err != nil {
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
	err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, neverAsked(t), attempt)
	if err == nil || !strings.Contains(err.Error(), "the fingerprint reader did not answer") {
		t.Fatalf("err = %v, want the reader-did-not-answer refusal", err)
	}
	if *calls != 1 {
		t.Fatalf("attempts = %d, want 1", *calls)
	}
}

// A reader another sudo holds: sudo does not get to ask it, the deploy waits.
func TestConfirmByFingerprintWaitsWhileTheReaderIsBusy(t *testing.T) {
	attempt, calls := scripted(step{out: notAsked}, step{out: notAsked}, step{ok: true})
	var log bytes.Buffer
	if err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &log, enrolledOK, attempt); err != nil {
		t.Fatalf("confirmByFingerprint: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("attempts = %d, want 3", *calls)
	}
	if !strings.Contains(log.String(), "looks busy (attempt 2)") {
		t.Errorf("log %q does not say the reader is busy", log.String())
	}
}

// No reader prints what a busy one does: only fprintd tells them apart.
func TestConfirmByFingerprintStopsWithNoFingerEnrolled(t *testing.T) {
	attempt, calls := scripted(step{out: notAsked})
	enrolled := func(context.Context) error { return errors.New("no devices available") }
	err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, enrolled, attempt)
	if err == nil || !strings.Contains(err.Error(), "the fingerprint reader did not answer (no devices available)") {
		t.Fatalf("err = %v, want the refusal with its reason", err)
	}
	if *calls != 1 {
		t.Fatalf("attempts = %d, want 1", *calls)
	}
}

// Another sudo error is no sign of a busy reader, even with a finger enrolled.
func TestConfirmByFingerprintStopsOnAnotherSudoError(t *testing.T) {
	attempt, calls := scripted(step{out: "someone is not in the sudoers file.\n"})
	err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, neverAsked(t), attempt)
	if err == nil || !strings.Contains(err.Error(), "the fingerprint reader did not answer") {
		t.Fatalf("err = %v, want the reader-did-not-answer refusal", err)
	}
	if *calls != 1 {
		t.Fatalf("attempts = %d, want 1", *calls)
	}
}

type clockedStep struct {
	took time.Duration
	out  string
	ok   bool
}

// clocked scripts attempts that each move a clock on by what the step took.
func clocked(steps ...clockedStep) (func(context.Context) (bool, []byte, error), func() time.Time, *int) {
	now := time.Unix(1000, 0)
	calls := 0
	return func(context.Context) (bool, []byte, error) {
		s := steps[calls]
		calls++
		now = now.Add(s.took)
		return s.ok, []byte(s.out), nil
	}, func() time.Time { return now }, &calls
}

func TestConfirmByFingerprintGivesUpOnAReaderBusyTooLong(t *testing.T) {
	attempt, now, calls := clocked(
		clockedStep{out: notAsked},
		clockedStep{took: time.Minute, out: notAsked},
		clockedStep{took: time.Minute, out: notAsked},
	)
	limits := fingerprintLimits{wait: time.Hour, busyWait: 2 * time.Minute, now: now}
	err := confirmByFingerprint(context.Background(), confirmTo, limits, &bytes.Buffer{}, enrolledOK, attempt)
	if err == nil || !strings.Contains(err.Error(), "did not answer (busy for 2m0s)") {
		t.Fatalf("err = %v, want the busy limit", err)
	}
	if *calls != 3 {
		t.Fatalf("attempts = %d, want 3", *calls)
	}
}

// The reader answering ends the run of busy attempts, even past the limit.
func TestConfirmByFingerprintReaderAnswerResetsTheBusyWait(t *testing.T) {
	attempt, now, calls := clocked(
		clockedStep{out: notAsked},
		clockedStep{took: 90 * time.Second, out: notAsked},
		clockedStep{took: 40 * time.Second, out: noMatch},
		clockedStep{took: time.Second, out: notAsked},
		clockedStep{took: 90 * time.Second, out: notAsked},
		clockedStep{took: time.Second, ok: true},
	)
	limits := fingerprintLimits{wait: time.Hour, busyWait: 2 * time.Minute, now: now}
	if err := confirmByFingerprint(context.Background(), confirmTo, limits, &bytes.Buffer{}, enrolledOK, attempt); err != nil {
		t.Fatalf("confirmByFingerprint: %v", err)
	}
	if *calls != 6 {
		t.Fatalf("attempts = %d, want 6", *calls)
	}
}

// A fprintd that does not answer does not outlive Ctrl-C.
func TestConfirmByFingerprintStopsOnCancelWhileAskingFprintd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempt, calls := scripted(step{out: notAsked})
	enrolled := func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	if err := confirmByFingerprint(ctx, confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, enrolled, attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if *calls != 1 {
		t.Fatalf("attempts = %d, want 1", *calls)
	}
}

// fakeFprintdList puts a fprintd-list running script first on PATH and a USER
// that is not the current user in the environment: tests never reach a real
// reader, and a name taken from the environment is seen.
func fakeFprintdList(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fprintd-list")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("USER", "not-the-current-user")
	return dir
}

func TestFingerprintEnrolledAsksForTheCurrentUser(t *testing.T) {
	dir := fakeFprintdList(t, `echo "$1" > "$(dirname "$0")/arg"; echo ' - #0: right-middle-finger'`)
	if err := fingerprintEnrolled(context.Background()); err != nil {
		t.Fatalf("fingerprintEnrolled: %v", err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "arg"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != u.Username {
		t.Errorf("fprintd-list asked about %q, want %q", got, u.Username)
	}
}

// A fprintd-list that hangs ends with the context, well before it would finish.
func TestFingerprintEnrolledStopsAHungFprintd(t *testing.T) {
	fakeFprintdList(t, "exec sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := fingerprintEnrolled(ctx); err == nil {
		t.Fatal("a killed fprintd-list counted as a listed finger")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("fingerprintEnrolled took %s, want it stopped with the context", took)
	}
}

func TestEnrolledFingers(t *testing.T) {
	if err := enrolledFingers([]byte(fingerListed), nil); err != nil {
		t.Errorf("a listed finger: %v", err)
	}
	if err := enrolledFingers([]byte("No devices available\n"), errors.New("exit status 1")); err == nil || !strings.Contains(err.Error(), "No devices available") {
		t.Errorf("no devices: err = %v", err)
	}
	if err := enrolledFingers([]byte("found 1 devices\nUser someone has no fingers enrolled\n"), nil); err == nil {
		t.Error("no enrolled finger: err = nil")
	}
}

func TestConfirmByFingerprintReturnsAFailedRun(t *testing.T) {
	runErr := errors.New("sudo: executable file not found")
	attempt, _ := scripted(step{out: timedOut, err: runErr})
	if err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, neverAsked(t), attempt); !errors.Is(err, runErr) {
		t.Fatalf("err = %v, want %v", err, runErr)
	}
}

func TestConfirmByFingerprintGivesUpAfterTheWait(t *testing.T) {
	// A sudo still waiting on the reader is killed when the wait runs out.
	attempt := func(ctx context.Context) (bool, []byte, error) {
		<-ctx.Done()
		return false, []byte("Place your right index finger on the fingerprint reader\n"), nil
	}
	err := confirmByFingerprint(context.Background(), confirmTo, limitsOf(20*time.Millisecond), &bytes.Buffer{}, neverAsked(t), attempt)
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
	if err := confirmByFingerprint(ctx, confirmTo, limitsOf(time.Minute), &bytes.Buffer{}, neverAsked(t), attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
