package envname

import "testing"

func TestSame(t *testing.T) {
	for _, tc := range []struct {
		goos string
		a, b string
		want bool
	}{
		{"linux", "GOMAXPROCS", "GOMAXPROCS", true},
		{"linux", "gomaxprocs", "GOMAXPROCS", false},
		{"windows", "GOMAXPROCS", "GOMAXPROCS", true},
		{"windows", "gomaxprocs", "GOMAXPROCS", true},
	} {
		if got := Same(tc.goos, tc.a, tc.b); got != tc.want {
			t.Errorf("Same(%q, %q, %q) = %v, want %v", tc.goos, tc.a, tc.b, got, tc.want)
		}
	}
}
