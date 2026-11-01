//go:build !linux

package iosched

import "context"

func configure(context.Context, int, string, Request) (Placement, error) { return Placement{}, nil }

// MemoryEvents has no counters to read off Linux.
func MemoryEvents(string) (map[string]uint64, error) { return nil, nil }
