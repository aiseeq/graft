package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/deploystate"
	"github.com/aiseeq/graft/v2/internal/lock"
)

// TestBeginMachineDeploymentChainsTheIdentity: a deploy started inside
// another names both in the environment its steps inherit, so an install
// under the inner one skips the outer one's marker too; restore puts the
// outer chain back, or clears it when there was none.
func TestBeginMachineDeploymentChainsTheIdentity(t *testing.T) {
	for _, outer := range []string{deploystate.Identity("/src/projectA", "test"), ""} {
		t.Run("outer "+outer, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv(deploystate.Env, outer)
			lk, err := lock.Acquire(context.Background(), filepath.Join(t.TempDir(), "deploy.lock"), time.Second, "test", os.Stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lk.Release() }()
			release, restore, err := beginMachineDeployment("/src/projectB", "test", lk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			want := deploystate.ChildIdentity(outer, "/src/projectB", "test")
			if got := os.Getenv(deploystate.Env); got != want {
				t.Errorf("%s = %q, want %q", deploystate.Env, got, want)
			}
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			got, set := os.LookupEnv(deploystate.Env)
			if got != outer || (outer == "" && set) {
				t.Errorf("after restore %s = %q (set %t), want %q", deploystate.Env, got, set, outer)
			}
		})
	}
}
