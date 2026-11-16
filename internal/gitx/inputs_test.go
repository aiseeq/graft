package gitx

import (
	"slices"
	"testing"

	"github.com/aiseeq/graft/v2/internal/testinputs"
)

// The processes this package's tests run read the environment, and go test
// keys a cached result on it only when the test reads it too.
func TestChildInputsKeyTheCache(t *testing.T) { testinputs.Env() }

func TestSameEnvName(t *testing.T) {
	wantDifferent := true
	if !SameEnvName("GOMAXPROCS", "GOMAXPROCS") || SameEnvName("gomaxprocs", "GOMAXPROCS") != !wantDifferent {
		t.Fatal("SameEnvName has incorrect platform case behavior")
	}
}

func TestWithoutNameCaseByOS(t *testing.T) {
	env := []string{"GOMAXPROCS=1", "gomaxprocs=2", "PATH=/bin"}
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"linux", []string{"gomaxprocs=2", "PATH=/bin"}},
		{"windows", []string{"PATH=/bin"}},
	} {
		if got := without(env, []string{"GOMAXPROCS"}, tc.goos); !slices.Equal(got, tc.want) {
			t.Errorf("without on %s = %#v, want %#v", tc.goos, got, tc.want)
		}
	}
}
