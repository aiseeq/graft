//go:build !windows

package execx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommandContextCancellationSendsTerm(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, err := CommandContext(ctx, time.Second, "sh", "-c",
		`trap 'echo term > "$1"; exit 0' TERM; : > "$2"; while :; do sleep 0.05; done`,
		"sh", marker, ready)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("command did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command did not stop")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "term\n" {
		t.Fatalf("TERM marker = %q, %v", data, err)
	}
}
