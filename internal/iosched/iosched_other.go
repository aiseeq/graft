//go:build !linux

package iosched

import (
	"context"
	"time"
)

func configure(context.Context, int, string, Request) (Placement, error) { return Placement{}, nil }

// MemoryEvents has no counters to read off Linux.
func MemoryEvents(string) (map[string]uint64, error) { return nil, nil }

// HalfMode has no CPU ceiling to set off Linux: the heavy scope it would cap
// and the /proc/stat the half is counted from are Linux's.
func HalfMode() (int64, int, time.Time, error) { return 0, 0, time.Time{}, nil }
