package memorymax

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		value   string
		bytes   int64
		percent int
		bad     bool
	}{
		{"12G", 12 << 30, 0, false}, {"512M", 512 << 20, 0, false}, {"40%", 0, 40, false},
		{"100%", 0, 100, false}, {"0%", 0, 0, true}, {"012G", 0, 0, true}, {"+5G", 0, 0, true}, {"8589934592G", 0, 0, true},
		{"0", 0, 0, true}, {"0G", 0, 0, true}, {"101%", 0, 0, true}, {"12GB", 0, 0, true}, {"-1G", 0, 0, true}, {"", 0, 0, true},
	} {
		b, p, err := Parse(tc.value)
		if (err != nil) != tc.bad || b != tc.bytes || p != tc.percent {
			t.Errorf("Parse(%q) = %d, %d, %v", tc.value, b, p, err)
		}
	}
}

func TestTotal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal:       16384 kB\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Total(root)
	if err != nil || got != 16<<20 {
		t.Fatalf("Total() = %d, %v; want %d", got, err, 16<<20)
	}
}
