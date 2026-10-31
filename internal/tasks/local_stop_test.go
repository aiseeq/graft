//go:build !windows

package tasks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalStopGraceCanBeShortenedForTest(t *testing.T) {
	oldGrace := localStopGrace
	localStopGrace = 80 * time.Millisecond
	defer func() { localStopGrace = oldGrace }()

	marker := filepath.Join(t.TempDir(), "finished")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' TERM; (sleep 0.4; : > "$1") & wait`, "sh", marker)
	r := &Runner{}
	r.stopLocalCommand(cmd, true)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command returned successfully")
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("cancelled command tree did not stop")
	}
	timer := time.NewTimer(500 * time.Millisecond)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("command reached its marker: %v", err)
			}
		case <-timer.C:
			return
		}
	}
}
