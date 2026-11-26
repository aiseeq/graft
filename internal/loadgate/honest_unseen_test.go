package loadgate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/lock"
)

func putAdmissionRecord(t *testing.T, path string, d reserveData) func() error {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), raw)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func TestMeasuredRateAboveCandidateCountsAtOnce(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "load.lock")
	release := putAdmissionRecord(t, path, reserveData{Cores: .5, Rate: 3, EMA: .05, At: now.Add(-time.Second).Format(time.RFC3339Nano), Admitted: now.Add(-3 * time.Second).Format(time.RFC3339Nano), Task: "measured"})
	defer release()
	got, err := unseenAdmittedCores(path, func(time.Time) float64 { return math.Exp(-time.Second.Seconds() / 60) })
	want := 2.95 * math.Exp(-1.0/60)
	if err != nil || got < 2.9 || math.Abs(got-want) > .01 {
		t.Fatalf("unseen = %.3f, %v; want about %.3f", got, err, want)
	}
}

func TestLateRampCountsFromTheRamp(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "load.lock")
	release := putAdmissionRecord(t, path, reserveData{Cores: .5, Rate: 8, EMA: .15, At: now.Add(-time.Second).Format(time.RFC3339Nano), Admitted: now.Add(-41 * time.Second).Format(time.RFC3339Nano), Task: "ramp"})
	defer release()
	got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
	want := (8 - .15) * math.Exp(-1.0/60)
	if err != nil || got < 7 || math.Abs(got-want) > .01 {
		t.Fatalf("unseen after ramp = %.3f, %v; want %.3f", got, err, want)
	}
}

func TestReservePollTracksEMAFromAdmission(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 18)
	setLoadavg(t, 10)
	oldPoll, oldWindow, oldReader, oldNow, oldPollNow := reservePollInterval, reserveWindow, reserveCPUReader, reserveAgeNow, reservePollNow
	reservePollInterval, reserveWindow = time.Millisecond, time.Second
	defer func() {
		reservePollInterval, reserveWindow, reserveCPUReader, reserveAgeNow, reservePollNow = oldPoll, oldWindow, oldReader, oldNow, oldPollNow
	}()
	base := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var clockMu sync.Mutex
	virtualNow := base
	reservePollNow = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return virtualNow
	}
	reserveAgeNow = reservePollNow
	var calls int
	var cpu float64
	reserveCPUReader = func(int) (float64, error) {
		clockMu.Lock()
		defer clockMu.Unlock()
		calls++
		rate := .3
		if calls >= 121 {
			rate = 8
		}
		virtualNow = virtualNow.Add(500 * time.Millisecond)
		cpu += rate * .5
		return cpu, nil
	}
	path := filepath.Join(t.TempDir(), "load.lock")
	admitted := base.Format(time.RFC3339Nano)
	initialRelease := putAdmissionRecord(t, path, reserveData{Cores: 4, Admitted: admitted, Task: "step-ramp"})
	var recordMu sync.Mutex
	var recordRelease func() error
	var latest reserveData
	published := make(chan reserveData, 1)
	onCPU := func(rate, ema float64, at time.Time) error {
		d := reserveData{Cores: 4, Rate: rate, EMA: ema, At: at.Format(time.RFC3339Nano), Admitted: admitted, Task: "step-ramp"}
		raw, err := json.Marshal(d)
		if err != nil {
			return err
		}
		release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), raw)
		if err != nil {
			return err
		}
		recordMu.Lock()
		old := recordRelease
		recordRelease, latest = release, d
		recordMu.Unlock()
		if old != nil {
			_ = old()
		}
		if rate >= 7 {
			select {
			case published <- d:
			default:
			}
		}
		return nil
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(done); <-stopped }) }
	go func() {
		defer close(stopped)
		runReservePoll("step-ramp", 4, 10*time.Second, reservePollInterval, reserveWindow, base, reservePollNow, reserveCPUReader, onCPU, nil, io.Discard, done, func(string) {}, func() {})
	}()
	defer func() {
		stop()
		recordMu.Lock()
		defer recordMu.Unlock()
		if recordRelease != nil {
			_ = recordRelease()
		}
		_ = initialRelease()
	}()
	select {
	case <-published:
		stop()
	case <-time.After(3 * time.Second):
		t.Fatal("reserve poll did not publish the 8-core rate")
	}
	recordMu.Lock()
	d := latest
	recordMu.Unlock()
	if math.Abs(d.Rate-8) > 1e-9 {
		t.Fatalf("published rate = %.6f; want 8", d.Rate)
	}
	gotAt, err := time.Parse(time.RFC3339Nano, d.At)
	if err != nil || !gotAt.Equal(base.Add(61*time.Second)) {
		t.Fatalf("published at = %s (%v); want admission + 61s", d.At, err)
	}
	wantEMA := .3 * (1 - math.Exp(-1))
	wantEMA = .3 + (wantEMA-.3)*math.Exp(-.5/60)
	wantEMA = 4.15 + (wantEMA-4.15)*math.Exp(-.5/60)
	if math.Abs(d.EMA-wantEMA) > 1e-5 {
		t.Fatalf("published ema = %.6f; want %.6f from the 0.3 → 8 ramp since admission", d.EMA, wantEMA)
	}
	unseen, err := unseenAdmittedCores(path, func(at time.Time) float64 {
		return math.Exp(-gotAt.Sub(at).Seconds() / 60)
	})
	if err != nil || math.Abs(unseen-(8-d.EMA)) > 1e-5 {
		t.Fatalf("head unseen = %.6f, %v; want published rate minus EMA %.6f", unseen, err, 8-d.EMA)
	}
	m := newMachine(testLoad, io.Discard, "next", "projectA:next")
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, 10, true, 18
	m.candidate, m.candidateKnown = 4, true
	if busy, err := m.reservesBusy(path); err != nil || busy.why != WhyReserve {
		t.Fatalf("head forecast = %+v, %v; want measured unseen to hold", busy, err)
	}
}

func TestUnseenReadsAdmittedRecordWithoutEMA(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "load.lock")
	release := putAdmissionRecord(t, path, reserveData{Cores: .5, ObservedCores: 4, ObservedAt: now.Add(-10 * time.Second).Format(time.RFC3339Nano), Admitted: now.Add(-30 * time.Second).Format(time.RFC3339Nano), Task: "legacy"})
	defer release()
	got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
	want := math.Max(.5*math.Exp(-.5), 4*math.Exp(-10.0/60))
	if err != nil || math.Abs(got-want) > .01 {
		t.Fatalf("legacy unseen = %.3f, %v; want %.3f", got, err, want)
	}
	if task, _, pending, err := firstUnmeasuredAdmission(path, now.Add(time.Second), time.Minute); err != nil || !pending || task != "legacy" {
		t.Fatalf("legacy first-measure guard = %q, %v, %v; want held", task, pending, err)
	}
}

func TestUnreadableCPUKeepsCandidateDecay(t *testing.T) {
	fakeMachine(t)
	oldPoll, oldWindow, oldReader, oldNow := reservePollInterval, reserveWindow, reserveCPUReader, reserveAgeNow
	reservePollInterval, reserveWindow = 5*time.Millisecond, 10*time.Millisecond
	var fail atomic.Bool
	var cpu float64
	var last = time.Now()
	reserveCPUReader = func(int) (float64, error) {
		if fail.Load() {
			return 0, io.ErrUnexpectedEOF
		}
		now := time.Now()
		cpu += 3 * now.Sub(last).Seconds()
		last = now
		return cpu, nil
	}
	defer func() {
		reservePollInterval, reserveWindow, reserveCPUReader, reserveAgeNow = oldPoll, oldWindow, oldReader, oldNow
	}()
	path := filepath.Join(t.TempDir(), "load.lock")
	_, release, err := Wait(context.Background(), "unreadable", "projectA:unreadable", nil, testLoad, path, io.Discard, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	deadline := time.Now().Add(time.Second)
	var d reserveData
	for time.Now().Before(deadline) {
		files, err := lock.LiveData(filepath.Dir(path), admittedPattern(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			var row reserveData
			_ = json.Unmarshal(file.Data, &row)
			if row.At != "" {
				d = row
				break
			}
		}
		if d.CPUError != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if d.At == "" {
		t.Fatal("reserve poll did not publish its first CPU sample")
	}
	fail.Store(true)
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		files, err := lock.LiveData(filepath.Dir(path), admittedPattern(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			var row reserveData
			_ = json.Unmarshal(file.Data, &row)
			if row.CPUError != "" {
				d = row
				break
			}
		}
		if d.CPUError != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if d.CPUError == "" {
		t.Fatal("read failure after measurement has no cpu_error")
	}
	at, err := time.Parse(time.RFC3339Nano, d.Admitted)
	if err != nil {
		t.Fatal(err)
	}
	virtualNow := at.Add(30 * time.Second)
	reserveAgeNow = func() time.Time { return virtualNow }
	got, err := unseenAdmittedCores(path, func(admitted time.Time) float64 { return math.Exp(-virtualNow.Sub(admitted).Seconds() / 60) })
	want := d.Cores * math.Exp(-.5)
	if err != nil || math.Abs(got-want) > want*.01 {
		t.Fatalf("unreadable unseen = %.3f, %v; want candidate decay %.3f", got, err, want)
	}
	if _, _, pending, err := firstUnmeasuredAdmission(path, at.Add(time.Millisecond), reserveWindow+reservePollInterval); err != nil || !pending {
		t.Fatalf("unreadable first measure pending=%v, err=%v; want held during grace", pending, err)
	}
}

func TestHalfModeForecastAfterHonestUnseen(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 11)
	setLoadavg(t, 9)
	oldPollNow, oldAgeNow := reservePollNow, reserveAgeNow
	base := time.Now().Add(-time.Minute)
	reservePollNow = func() time.Time { return base }
	reserveAgeNow = func() time.Time { return base }
	defer func() { reservePollNow, reserveAgeNow = oldPollNow, oldAgeNow }()
	path := filepath.Join(t.TempDir(), "load.lock")
	admitted := base.Add(-2 * time.Second).Format(time.RFC3339Nano)
	var cpu float64
	reads := 0
	var recordRelease func() error
	readCPU := func(int) (float64, error) {
		reads++
		if reads == 1 {
			return 0, nil
		}
		cpu = 1.5
		return cpu, nil
	}
	state := reservePollState{lastEMAAt: base, ema: .1, lastRate: .1, haveRate: true}
	onCPU := func(rate, ema float64, at time.Time) error {
		raw, err := json.Marshal(reserveData{Cores: .5, Rate: rate, EMA: ema, At: at.Format(time.RFC3339Nano), Admitted: admitted, Task: "underestimated"})
		if err != nil {
			return err
		}
		release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), raw)
		if err != nil {
			return err
		}
		if recordRelease != nil {
			_ = recordRelease()
		}
		recordRelease = release
		return nil
	}
	defer func() {
		if recordRelease != nil {
			_ = recordRelease()
		}
	}()
	state.handleTick("underestimated", .5, time.Second, readCPU, onCPU, nil, func(string) {}, func() {}, io.Discard)
	base = base.Add(time.Second)
	state.handleTick("underestimated", .5, time.Second, readCPU, onCPU, nil, func(string) {}, func() {}, io.Discard)
	releases, err := lock.LiveData(filepath.Dir(path), admittedPattern(path))
	if err != nil || len(releases) != 1 {
		t.Fatalf("poll published %d admission records, %v; want one", len(releases), err)
	}
	m := newMachine(testLoad, io.Discard, "next", "projectA:next")
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, 9, true, 11
	m.candidate, m.candidateKnown = 1, true
	b, err := m.reservesBusy(path)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("half-mode forecast = %+v, %v; want honest unseen to hold candidate", b, err)
	}
}

func TestSimultaneousRampWithDelayedPublish(t *testing.T) {
	setRunnable, _, _ := fakeMachine(t)
	setRunnable(1)
	setRunnableLimit(t, 18)
	setLoadavg(t, 10)
	now := time.Now()
	path := filepath.Join(t.TempDir(), "load.lock")
	var releases []func() error
	for i := range 2 {
		admitted := now.Add(-41*time.Second + time.Duration(i)*time.Millisecond)
		ema := .3 * (1 - math.Exp(-40.0/60))
		releases = append(releases, putAdmissionRecord(t, path, reserveData{Cores: .3, Rate: .3, EMA: ema, At: now.Add(-time.Second).Format(time.RFC3339Nano), Admitted: admitted.Format(time.RFC3339Nano), Task: fmt.Sprintf("ramp-%d", i)}))
	}
	defer func() {
		for _, release := range releases {
			_ = release()
		}
	}()
	m := newMachine(testLoad, io.Discard, "head", "projectA:head")
	m.runnable, m.avg1, m.avg1Known, m.runnableLimit = 1, 9, true, 18
	m.candidate, m.candidateKnown = 4, true
	b, err := m.reservesBusy(path)
	if err != nil || b.why != "" {
		t.Fatalf("first head during delayed publication = %+v, %v; want at most one task admitted", b, err)
	}
	newAdmission := now.Add(-500 * time.Millisecond)
	newRelease := putAdmissionRecord(t, path, reserveData{Cores: 4, Admitted: newAdmission.Format(time.RFC3339Nano), Task: "first-head"})
	defer newRelease()
	b, err = m.reservesBusy(path)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("second head during first CPU window = %+v, %v; want first CPU hold", b, err)
	}
	_ = newRelease()
	for _, release := range releases {
		_ = release()
	}
	releases = nil
	for i := range 2 {
		admitted := now.Add(-41*time.Second + time.Duration(i)*time.Millisecond)
		releases = append(releases, putAdmissionRecord(t, path, reserveData{Cores: .3, Rate: 6, EMA: .1, At: now.Format(time.RFC3339Nano), Admitted: admitted.Format(time.RFC3339Nano), Task: fmt.Sprintf("ramp-%d", i)}))
	}
	if task, _, pending, err := firstUnmeasuredAdmission(path, now, reserveWindow+reservePollInterval); err != nil || pending {
		t.Fatalf("published ramp first-measure guard = %q, %v, %v; want no pause", task, pending, err)
	}
	unseen, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
	if err != nil || unseen < 11.5 {
		t.Fatalf("published ramp unseen = %.3f, %v; want at least 11.5", unseen, err)
	}
	b, err = m.reservesBusy(path)
	if err != nil || b.why != WhyReserve {
		t.Fatalf("head after delayed publication = %+v, %v; want honest unseen to hold", b, err)
	}
}

func TestUnseenSurvivesHeadRestart(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "load.lock")
	release := putAdmissionRecord(t, path, reserveData{Cores: .5, Rate: 3, EMA: .2, At: now.Add(-5 * time.Second).Format(time.RFC3339Nano), Admitted: now.Add(-20 * time.Second).Format(time.RFC3339Nano), Task: "persistent"})
	defer release()
	share := func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) }
	want, err := unseenAdmittedCores(path, share)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUnseenRestartReader$")
	cmd.Env = append(os.Environ(), "GRAFT_UNSEEN_RESTART_PATH="+path, "GRAFT_UNSEEN_RESTART_NOW="+now.Format(time.RFC3339Nano))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restart reader failed: %v\n%s", err, out)
	}
	var got float64
	for line := range strings.SplitSeq(string(out), "\n") {
		if after, ok := strings.CutPrefix(line, "RESTART_UNSEEN="); ok {
			if _, err := fmt.Sscanf(after, "%f", &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("restart forecast changed: %.9f; want %.9f\n%s", got, want, out)
	}
}

func TestUnseenRestartReader(t *testing.T) {
	path := os.Getenv("GRAFT_UNSEEN_RESTART_PATH")
	if path == "" {
		return
	}
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("GRAFT_UNSEEN_RESTART_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESTART_UNSEEN=%.9f\n", got)
}

func TestCorruptAdmittedRecordCountsAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), []byte("{broken"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	now := time.Now()
	got, err := unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
	if err != nil || got < 3.99 {
		t.Fatalf("corrupt unseen = %.3f, %v; want conservative unknown near 4", got, err)
	}
	if task, _, pending, err := firstUnmeasuredAdmission(path, now, time.Minute); err != nil || !pending || task != "unknown task" {
		t.Fatalf("corrupt first-measure guard = %q, %v, %v; want unknown task held", task, pending, err)
	}
}

func TestCorruptAdmittedRecordAgesOut(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "malformed JSON", data: []byte("{broken")},
		{name: "invalid admission time", data: []byte(`{"cores":0.5,"admitted":"not-a-time","task":"bad-time"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.lock")
			release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), test.data)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			files, err := filepath.Glob(admittedFilesPattern(path))
			if err != nil || len(files) != 1 {
				t.Fatalf("corrupt admitted files = %v, %v", files, err)
			}
			base := time.Now().Add(-2 * time.Minute)
			if err := os.Chtimes(files[0], base, base); err != nil {
				t.Fatal(err)
			}
			grace := time.Minute
			if task, _, pending, err := firstUnmeasuredAdmission(path, base, grace); err != nil || !pending || task != "unknown task" {
				t.Fatalf("fresh corrupt pause = %q, %v, %v; want held", task, pending, err)
			}
			now := base.Add(30 * time.Second)
			oldAgeNow := reserveAgeNow
			reserveAgeNow = func() time.Time { return now }
			defer func() { reserveAgeNow = oldAgeNow }()
			got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
			want := 4 * math.Exp(-30.0/60)
			if err != nil || math.Abs(got-want) > 1e-12 {
				t.Fatalf("corrupt unseen at mtime age = %.12f, %v; want candidate decay %.12f", got, err, want)
			}
			if task, _, pending, err := firstUnmeasuredAdmission(path, base.Add(grace+time.Second), grace); err != nil || pending {
				t.Fatalf("old corrupt pause = %q, %v, %v; want released", task, pending, err)
			}
		})
	}
}

func TestFutureMtimeDoesNotExtendAdmissionGraceOrUnseenReserve(t *testing.T) {
	for _, record := range []struct {
		name string
		data []byte
	}{
		{name: "malformed JSON", data: []byte("{broken")},
		{name: "invalid admission time", data: []byte(`{"cores":0.5,"admitted":"not-a-time","task":"bad-time"}`)},
	} {
		t.Run(record.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.lock")
			release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), record.data)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			files, err := filepath.Glob(admittedFilesPattern(path))
			if err != nil || len(files) != 1 {
				t.Fatalf("future mtime files = %v, %v", files, err)
			}
			future := time.Now().Add(365 * 24 * time.Hour)
			if err := os.Chtimes(files[0], future, future); err != nil {
				t.Fatal(err)
			}
			grace := time.Minute
			now := time.Now()
			if _, retry, pending, err := firstUnmeasuredAdmission(path, now, grace); err != nil || pending {
				t.Fatalf("future mtime pause = %v, %v, %v; want stale record released", retry, pending, err)
			}
			if _, _, pending, err := firstUnmeasuredAdmission(path, now.Add(grace+time.Second), grace); err != nil || pending {
				t.Fatalf("future mtime still pauses after grace: pending=%v, err=%v", pending, err)
			}
			oldAgeNow := reserveAgeNow
			reserveAgeNow = func() time.Time { return now }
			defer func() { reserveAgeNow = oldAgeNow }()
			got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-max(now.Sub(at), 0).Seconds() / 60) })
			if err != nil || got > 1e-9 {
				t.Fatalf("future mtime unseen = %.9f, %v; want stale reserve near zero", got, err)
			}
		})
	}
}

func TestInvalidAdmissionTimeCountsAsUnknownAndPauses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	release := putAdmissionRecord(t, path, reserveData{Cores: .5, Admitted: "not-a-time", Task: "bad-time"})
	defer release()
	now := time.Now()
	got, err := unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
	if err != nil || got < 3.99 {
		t.Fatalf("invalid admission unseen = %.3f, %v; want unknown 4-core candidate", got, err)
	}
	if task, _, pending, err := firstUnmeasuredAdmission(path, now, time.Minute); err != nil || !pending || task != "unknown task" {
		t.Fatalf("invalid admission pause = %q, %v, %v; want held", task, pending, err)
	}
}

func TestInvalidObservationTimesUseUnmeasuredForecast(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		data reserveData
		want float64
	}{
		{
			name: "observed_at",
			data: reserveData{Cores: 2, Rate: 8, EMA: 0, At: now.Format(time.RFC3339Nano), ObservedCores: 3, ObservedAt: "not-a-time", Admitted: now.Format(time.RFC3339Nano), Task: "bad-observed-at"},
			want: 2,
		},
		{
			name: "at",
			data: reserveData{Cores: 2, Rate: 8, EMA: 0, At: "not-a-time", ObservedCores: 3, ObservedAt: now.Add(-time.Second).Format(time.RFC3339Nano), Admitted: now.Add(-10 * time.Second).Format(time.RFC3339Nano), Task: "bad-rate-at"},
			want: max(2*math.Exp(-10.0/60), 3*math.Exp(-1.0/60)),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.lock")
			release := putAdmissionRecord(t, path, tc.data)
			defer release()
			got, err := unseenAdmittedCores(path, func(at time.Time) float64 { return math.Exp(-now.Sub(at).Seconds() / 60) })
			if err != nil || math.Abs(got-tc.want) > .01 {
				t.Fatalf("unseen = %.3f, %v; want unmeasured forecast %.3f", got, err, tc.want)
			}
			if task, _, pending, err := firstUnmeasuredAdmission(path, now, time.Minute); err != nil || !pending || task == "" {
				t.Fatalf("first CPU guard = %q, %v, %v; want malformed measurement held", task, pending, err)
			}
		})
	}
}

func TestMalformedAndMeasuredRecordsSameAdmissionFallback(t *testing.T) {
	now := time.Now()
	admitted := now.Format(time.RFC3339Nano)
	tests := []struct {
		name string
		bad  reserveData
	}{
		{
			name: "observed_at",
			bad:  reserveData{Cores: 4, Rate: 8, EMA: 0, At: now.Format(time.RFC3339Nano), ObservedCores: 4, ObservedAt: "not-a-time", Admitted: admitted, Task: "bad-observed"},
		},
		{
			name: "at",
			bad:  reserveData{Cores: 4, Rate: 8, EMA: 0, At: "not-a-time", ObservedCores: 4, ObservedAt: now.Format(time.RFC3339Nano), Admitted: admitted, Task: "bad-at"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.lock")
			badRelease := putAdmissionRecord(t, path, tc.bad)
			defer badRelease()
			goodRelease := putAdmissionRecord(t, path, reserveData{Cores: 4, Rate: 8, EMA: 7.9, At: now.Format(time.RFC3339Nano), ObservedCores: 4, ObservedAt: now.Format(time.RFC3339Nano), Admitted: admitted, Task: "good-measure"})
			defer goodRelease()
			got, err := unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
			if err != nil || math.Abs(got-4) > 1e-9 {
				t.Fatalf("mixed-record unseen = %.3f, %v; want unmeasured candidate fallback 4", got, err)
			}
		})
	}
}

func TestInvalidAdmissionTimestampRecordsCountSeparately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "load.lock")
	first := putAdmissionRecord(t, path, reserveData{Cores: 1, Admitted: "same-invalid-time", Task: "first-unknown"})
	defer first()
	second := putAdmissionRecord(t, path, reserveData{Cores: 1, Admitted: "same-invalid-time", Task: "second-unknown"})
	defer second()
	got, err := unseenAdmittedCores(path, func(time.Time) float64 { return 1 })
	if err != nil || math.Abs(got-8) > 1e-9 {
		t.Fatalf("unseen for duplicate invalid timestamps = %.3f, %v; want two independent 4-core fallbacks", got, err)
	}
}

func TestDisappearedAdmissionRecordIsSkipped(t *testing.T) {
	oldStat := admittedRecordStat
	admittedRecordStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	defer func() { admittedRecordStat = oldStat }()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "bad_json", data: []byte("{broken")},
		{name: "bad_admitted", data: []byte(`{"cores":1,"admitted":"not-a-time","task":"gone"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "load.lock")
			release, err := lock.Reserve(filepath.Dir(path), admittedPattern(path), tc.data)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if got, err := unseenAdmittedCores(path, func(time.Time) float64 { return 1 }); err != nil || got != 0 {
				t.Fatalf("unseen after stat ENOENT = %.3f, %v; want skipped record", got, err)
			}
			if task, _, pending, err := firstUnmeasuredAdmission(path, time.Now(), time.Minute); err != nil || pending {
				t.Fatalf("first CPU after stat ENOENT = %q, %v, %v; want skipped record", task, pending, err)
			}
		})
	}
}

func TestRateCrossingHalfCandidatePublishes(t *testing.T) {
	start := time.Now()
	var mu sync.Mutex
	rate, cpu, last := 1.9, 0.0, start
	readCPU := func(int) (float64, error) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		cpu += rate * now.Sub(last).Seconds()
		last = now
		return cpu, nil
	}
	writes, drops := 0, 0
	state := reservePollState{lastEMAAt: start}
	onCPU := func(float64, float64, time.Time) error { writes++; return nil }
	for i := range 40 {
		if i == 20 {
			mu.Lock()
			rate = 2.3
			mu.Unlock()
		}
		state.handleTick("crossing", 4, 20*time.Millisecond, readCPU, onCPU, nil, func(string) {}, func() { drops++ }, io.Discard)
		time.Sleep(5 * time.Millisecond)
	}
	if !state.reserveDropped || writes == 0 || drops != 1 {
		t.Fatalf("published=%d drops=%d reserveDropped=%v; want a published rate before the single release", writes, drops, state.reserveDropped)
	}
}

func TestCPUErrorIsRecordedAgainAfterRecovery(t *testing.T) {
	fail := true
	var cpu float64
	last := time.Now()
	readCPU := func(int) (float64, error) {
		if fail {
			return 0, io.ErrUnexpectedEOF
		}
		now := time.Now()
		cpu += 3 * now.Sub(last).Seconds()
		last = now
		return cpu, nil
	}
	errorsSeen := 0
	onError := func(error) error { errorsSeen++; return nil }
	state := reservePollState{lastEMAAt: time.Now()}
	tick := func() {
		state.handleTick("error-episodes", 1, 20*time.Millisecond, readCPU, func(float64, float64, time.Time) error { return nil }, onError, func(string) {}, func() {}, io.Discard)
		time.Sleep(5 * time.Millisecond)
	}
	tick()
	fail = false
	for range 8 {
		tick()
	}
	fail = true
	tick()
	if errorsSeen != 2 {
		t.Fatalf("cpu_error writes=%d after two error episodes; want 2", errorsSeen)
	}
}

func TestCPUErrorRecoveryPublishesSameRate(t *testing.T) {
	fail := false
	var cpu float64
	last := time.Now()
	readCPU := func(int) (float64, error) {
		if fail {
			return 0, io.ErrUnexpectedEOF
		}
		now := time.Now()
		cpu += 3 * now.Sub(last).Seconds()
		last = now
		return cpu, nil
	}
	cpuErrorPresent, writes := false, 0
	onCPU := func(float64, float64, time.Time) error { cpuErrorPresent = false; writes++; return nil }
	onError := func(error) error { cpuErrorPresent = true; return nil }
	state := reservePollState{lastEMAAt: time.Now(), haveObserved: true, observed: 3}
	tick := func() {
		state.handleTick("recover-same-rate", 4, 20*time.Millisecond, readCPU, onCPU, onError, func(string) {}, func() {}, io.Discard)
		time.Sleep(5 * time.Millisecond)
	}
	for range 8 {
		tick()
	}
	fail = true
	tick()
	if !cpuErrorPresent {
		t.Fatal("read failure did not set cpu_error")
	}
	fail = false
	before := writes
	for range 8 {
		tick()
	}
	if cpuErrorPresent || writes == before {
		t.Fatalf("recovery writes=%d (+%d), cpu_error=%v; want same-rate record without cpu_error", writes, writes-before, cpuErrorPresent)
	}
}

func TestCPUErrorRecoveryPublishesBelowHalfCandidate(t *testing.T) {
	oldPollNow := reservePollNow
	base := time.Now()
	reservePollNow = func() time.Time { return base }
	defer func() { reservePollNow = oldPollNow }()
	fail := true
	var cpu float64
	readCPU := func(int) (float64, error) {
		if fail {
			return 0, io.ErrUnexpectedEOF
		}
		cpu += 1.5
		return cpu, nil
	}
	writes := 0
	var published float64
	onCPU := func(rate, _ float64, _ time.Time) error {
		writes++
		published = rate
		return nil
	}
	state := reservePollState{haveObserved: true, observed: 1.5}
	state.handleTick("below-half", 4, time.Second, readCPU, onCPU, nil, func(string) {}, func() {}, io.Discard)
	fail = false
	state.handleTick("below-half", 4, time.Second, readCPU, onCPU, nil, func(string) {}, func() {}, io.Discard)
	base = base.Add(time.Second)
	state.handleTick("below-half", 4, time.Second, readCPU, onCPU, nil, func(string) {}, func() {}, io.Discard)
	if writes != 1 || published != 1.5 || state.reserveDropped {
		t.Fatalf("recovery publication writes=%d rate=%.2f dropped=%v; want one 1.5-core update without dropping the 4-core reserve", writes, published, state.reserveDropped)
	}
}
