// Package envname provides environment variable name comparisons.
package envname

import (
	"runtime"
	"strings"
)

// Same reports whether a and b name the same environment variable on goos.
func Same(goos, a, b string) bool {
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SameEnvName reports whether a and b name one environment variable on this OS.
func SameEnvName(a, b string) bool {
	return Same(runtime.GOOS, a, b)
}
