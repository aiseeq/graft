//go:build windows

package proc

import (
	"context"
	"time"
)

func CallerGoneInterval() (time.Duration, error) { return time.Second, nil }
func WatchCaller(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
