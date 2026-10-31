package tasks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestTaskReserveSecondsEndAtCPURelease(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	start := time.Now()
	stopped := time.Time{}
	var stderr bytes.Buffer
	r := &Runner{
		Root:       t.TempDir(),
		Stats:      stats.NewRecorder(io.Discard),
		Stderr:     &stderr,
		noRepoName: true,
		frames:     []*frame{{task: "synthetic", start: start}},
	}
	f := r.frames[0]
	f.reserveCores = 1
	f.reserveSince = start
	f.reserveEnd = func() string { return "cpu" }
	f.reserveStoppedAt = func() time.Time { return stopped }
	f.reserveRelease = func() error { return nil }
	time.Sleep(50 * time.Millisecond) // synthetic CPU release after the sample window
	stopped = time.Now()
	time.Sleep(80 * time.Millisecond) // the task keeps running after its reservation ends
	r.end(f, false)

	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var rec stats.Record
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if rec.ReserveEnd != "cpu" || rec.WallS < 0.1 {
		t.Fatalf("record = %+v; want CPU release and task lasting longer", rec)
	}
	if rec.ReserveS < 0.04 || rec.ReserveS > 0.075 {
		t.Fatalf("reserve_s = %.3f; want about 0.05s, before task end", rec.ReserveS)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
