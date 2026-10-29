package tasks

import (
	"fmt"
	"testing"
)

func TestTailCaptureReason(t *testing.T) {
	var capture tailCapture
	for i := range 15 {
		fmt.Fprintf(&capture, "line %d\n", i)
	}
	if got := capture.reason(); got != "line 5\nline 6\nline 7\nline 8\nline 9\nline 10\nline 11\nline 12\nline 13\nline 14" {
		t.Fatalf("tail reason: %q", got)
	}
}

func TestGoCheckNoOutput(t *testing.T) {
	for _, c := range []struct {
		argv, args []string
		want       bool
	}{
		{[]string{"go", "test", "./..."}, nil, true},
		{[]string{"go", "vet", "./..."}, nil, true},
		{[]string{"go", "build", "./..."}, nil, false},
		{[]string{"go", "test", "-coverprofile=c.out", "./..."}, nil, false},
		{[]string{"go", "test", "./...", "-coverprofile", "c.out"}, nil, false},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, nil, true},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-run", "TestX"}, true},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-coverprofile=c.out"}, false},
		{[]string{"go", "test", "-v", ArgsPlaceholder, "./..."}, []string{"-test.coverprofile=c.out"}, false},
		{[]string{"go", "test", ArgsPlaceholder}, []string{".", "-args", "-test.cpuprofile=c.out"}, false},
		{[]string{"go", "test", "-" + ArgsPlaceholder, "./..."}, []string{"coverprofile=c.out"}, false},
		{[]string{"go", "test", "-run=" + ArgsPlaceholder, "./..."}, []string{"-o"}, true},
		{[]string{"go", "test", "-fuzz=FuzzX", "./..."}, nil, false},
		{[]string{"go", "test", "-mod=mod", "./..."}, nil, false},
		{[]string{"go", "test", "-mod=readonly", "./..."}, nil, true},
	} {
		if got := goCheckNoOutput(c.argv, c.args); got != c.want {
			t.Errorf("goCheckNoOutput(%q, %q) = %v, want %v", c.argv, c.args, got, c.want)
		}
	}
}
