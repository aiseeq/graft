package app

import (
	"os"
	"testing"
)

func TestIsolateMergeGitEnvironmentRestoresUnsetVariablesAsUnset(t *testing.T) {
	names := []string{"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS"}
	type previous struct {
		value string
		set   bool
	}
	old := make(map[string]previous, len(names))
	for _, name := range names {
		value, set := os.LookupEnv(name)
		old[name] = previous{value: value, set: set}
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for name, state := range old {
			if state.set {
				if err := os.Setenv(name, state.value); err != nil {
					t.Errorf("restore %s: %v", name, err)
				}
			} else {
				if err := os.Unsetenv(name); err != nil {
					t.Errorf("unset %s: %v", name, err)
				}
			}
		}
	})

	restore, err := isolateMergeGitEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GIT_INDEX_FILE", "injected-index"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GIT_NO_REPLACE_OBJECTS", "1"); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("previously unset %s remained set after restore", name)
		}
	}
}

func TestIsolateMergeGitEnvironmentRestoresPreviouslySetValues(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", "original-index")
	t.Setenv("GIT_NO_REPLACE_OBJECTS", "original-replace-setting")

	restore, err := isolateMergeGitEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("%s remained set while isolated", name)
		}
	}
	if err := os.Setenv("GIT_INDEX_FILE", "injected-index"); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("GIT_NO_REPLACE_OBJECTS", "injected-replace-setting"); err != nil {
		t.Fatal(err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"GIT_INDEX_FILE":         "original-index",
		"GIT_NO_REPLACE_OBJECTS": "original-replace-setting",
	} {
		if got, ok := os.LookupEnv(name); !ok || got != want {
			t.Errorf("%s after restore = %q, set=%t; want %q", name, got, ok, want)
		}
	}
}
