package testinputs

import (
	"testing"
)

func TestPerRunEnvNameCaseByOS(t *testing.T) {
	for _, tc := range []struct {
		goos string
		name string
		want bool
	}{
		{"linux", "GOMAXPROCS", false},
		{"linux", "gomaxprocs", true},
		{"windows", "GOMAXPROCS", false},
		{"windows", "gomaxprocs", false},
	} {
		if got := shouldReadEnv(tc.name, tc.goos); got != tc.want {
			t.Errorf("shouldReadEnv(%q, %q) = %v, want %v", tc.name, tc.goos, got, tc.want)
		}
	}
}

func TestEnvReadsInheritedAndNewlySetInputs(t *testing.T) {
	var reads []string
	readEnv([]string{
		"gomaxprocs=2",
		"GOMAXPROCS=4",
		"GRAFT_RUN_ID=test-run",
		"GRAFT_CHILD_WAITS=/tmp/graft-child-waits-1",
		"PATH=/bin",
	}, func(name string) string {
		reads = append(reads, name)
		return ""
	})

	read := make(map[string]bool, len(reads))
	for _, name := range reads {
		read[name] = true
	}
	// On Windows gomaxprocs is GOMAXPROCS, a perRun name; elsewhere it is a
	// variable of its own.
	wantRead, wantSkipped := []string{"GIT_AUTHOR_NAME", "PATH"}, []string{"GOMAXPROCS", "GRAFT_RUN_ID", "GRAFT_CHILD_WAITS"}

	wantRead = append(wantRead, "gomaxprocs")

	for _, name := range wantRead {
		if !read[name] {
			t.Errorf("readEnv did not read %q; reads: %v", name, reads)
		}
	}
	for _, name := range wantSkipped {
		if read[name] {
			t.Errorf("readEnv read perRun variable %q", name)
		}
	}
}
