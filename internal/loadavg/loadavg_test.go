package loadavg

import (
	"os"
	"path/filepath"
	"testing"
)

func fake(t *testing.T, env, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env, p)
}

// TestIOFullAndSwapPages read the kernel's formats and refuse what lacks
// the figure.
func TestIOFullAndSwapPages(t *testing.T) {
	fake(t, PressureEnv, "some avg10=1.50 avg60=4.20 avg300=4.34 total=3708045266\nfull avg10=0.67 avg60=3.19 avg300=3.00 total=3283949798\n")
	if got, err := IOFull(); err != nil || got != 3.19 {
		t.Errorf("IOFull = %v, %v", got, err)
	}
	fake(t, PressureEnv, "some avg10=1.50 avg60=4.20 avg300=4.34 total=1\n")
	if _, err := IOFull(); err == nil {
		t.Error("no full line accepted")
	}
	fake(t, VMStatEnv, "nr_free_pages 12\npswpin 100\npswpout 23\npgfault 9\n")
	if got, err := SwapPages(); err != nil || got != 123 {
		t.Errorf("SwapPages = %v, %v", got, err)
	}
	fake(t, VMStatEnv, "pswpin 100\n")
	if _, err := SwapPages(); err == nil {
		t.Error("vmstat without pswpout accepted")
	}
}
