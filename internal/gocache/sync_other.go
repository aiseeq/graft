//go:build !linux

package gocache

func syncCacheFS(string) error { return nil }
