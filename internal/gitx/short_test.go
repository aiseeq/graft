package gitx

import "testing"

func TestShortKeepsShortInput(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789abcdef0123": "0123456789ab",
		"0123456789ab":         "0123456789ab",
		"abc":                  "abc",
		"":                     "",
	} {
		if got := Short(in); got != want {
			t.Errorf("Short(%q) = %q, want %q", in, got, want)
		}
	}
}
