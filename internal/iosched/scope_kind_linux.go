//go:build linux

package iosched

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aiseeq/graft/v2/internal/atomicfile"
)

func recordScopeKind(scope, kind string) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		fmt.Fprintf(os.Stderr, "graft: heavy scope kind not recorded: XDG_RUNTIME_DIR is not set\n")
		return
	}
	dir := filepath.Join(runtimeDir, "graft", "heavy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "graft: heavy scope kind not recorded: %v\n", err)
		return
	}
	now := time.Now()
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "graft: heavy scope kind cleanup failed: %v\n", err)
	} else {
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				fmt.Fprintf(os.Stderr, "graft: heavy scope kind cleanup failed: %v\n", err)
				continue
			}
			if now.Sub(info.ModTime()) > 24*time.Hour {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					fmt.Fprintf(os.Stderr, "graft: heavy scope kind cleanup failed: %v\n", err)
				}
			}
		}
	}
	if err := atomicfile.Write(filepath.Join(dir, scope), []byte(kind+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "graft: heavy scope kind not recorded: %v\n", err)
	}
}
