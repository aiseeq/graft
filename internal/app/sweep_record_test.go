package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gitx"
)

func TestWriteSweepRecordDoesNotTruncateOpenTarget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	repo := &gitx.Repo{CommonDir: t.TempDir()}
	path, err := sweepPath(repo, "deployed", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const oldRecord = "old-head\nold-held\n"
	if err := os.WriteFile(path, []byte(oldRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := writeSweepRecord(repo, "prod", "new-head", []string{"new-held"}); err != nil {
		t.Fatal(err)
	}
	gotOld, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOld) != oldRecord {
		t.Fatalf("reader of the previous target record got %q, want complete old record %q", gotOld, oldRecord)
	}
	gotNew, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotNew) != "new-head\nnew-held\n" {
		t.Fatalf("target record = %q, want complete new pair", gotNew)
	}
}

func TestSweepStampFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stamp")
	if fresh, err := sweepStampFresh(path, false); err != nil || fresh {
		t.Fatalf("missing stamp: fresh=%v err=%v, want not fresh", fresh, err)
	}
	if err := writeSweepStamp(path, false); err != nil {
		t.Fatal(err)
	}
	if fresh, err := sweepStampFresh(path, false); err != nil || !fresh {
		t.Fatalf("stamp of the same mode: fresh=%v err=%v, want fresh", fresh, err)
	}
	if fresh, _ := sweepStampFresh(path, true); fresh {
		t.Fatal("stamp of the other mode is fresh, want a sweep at once")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := sweepStampFresh(path, false); fresh {
		t.Fatal("empty stamp of the old format is fresh, want a sweep")
	}
	if err := writeSweepStamp(path, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Skip("running as root: an unreadable stamp cannot be made")
	}
	if fresh, err := sweepStampFresh(path, false); err == nil || fresh {
		t.Fatalf("unreadable stamp: fresh=%v err=%v, want an error and a sweep", fresh, err)
	}
	if err := writeSweepStamp(path, true); err != nil {
		t.Fatalf("an unreadable stamp is not replaced: %v", err)
	}
	if fresh, err := sweepStampFresh(path, true); err != nil || !fresh {
		t.Fatalf("replaced stamp: fresh=%v err=%v, want fresh", fresh, err)
	}
}

func TestWriteSweepStampDoesNotTruncateOpenStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stamp")
	if err := writeSweepStamp(path, false); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := writeSweepStamp(path, true); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "false\n" {
		t.Fatalf("reader of the previous stamp got %q, want the whole old stamp", got)
	}
}
