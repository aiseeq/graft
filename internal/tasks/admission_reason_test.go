package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/loadgate"
	"github.com/aiseeq/graft/v2/internal/stats"
)

func TestAdmissionRefusalHasReason(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	cfg, err := config.Parse([]byte("schema: 1\nversion: {mode: none}\ntasks:\n  heavy:\n    heavy: true\n    run: [[true]]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Root: root, Config: cfg, Stats: stats.NewRecorder(os.Stderr), Stderr: io.Discard, Stdout: io.Discard, noRepoName: true,
		admitTask: func(context.Context, string) (loadgate.Admission, func() error, error) {
			return loadgate.Admission{}, nil, errors.New("synthetic admission refused")
		},
	}
	err = r.run(context.Background(), "heavy", nil)
	if err == nil || err.Error() != "synthetic admission refused" {
		t.Fatalf("run error = %v", err)
	}
	path, err := stats.Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var records []stats.Record
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var rec stats.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Kind == stats.KindTask && rec.Task == "heavy" {
			records = append(records, rec)
		}
	}
	if len(records) != 1 || records[0].Reason != "synthetic admission refused" {
		t.Fatalf("task records = %+v; want refusal reason", records)
	}
}
