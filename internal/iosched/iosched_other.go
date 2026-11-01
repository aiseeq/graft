//go:build !linux

package iosched

import "context"

func enter(context.Context, int, string) (string, error) { return "", nil }
