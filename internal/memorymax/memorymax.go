// Package memorymax parses task memory ceilings and reads the machine's RAM.
package memorymax

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var errSyntax = errors.New("want a size such as 12G (binary K, M, G) or a percentage from 1% to 100%")

var shifts = map[byte]uint{'K': 10, 'M': 20, 'G': 30}

// Parse reads a ceiling: a size in bytes, or a percentage of MemTotal, the
// other result 0. Only canonical numbers pass: no sign, no leading zero.
func Parse(value string) (bytes int64, percent int, err error) {
	if digits, ok := strings.CutSuffix(value, "%"); ok {
		n, err := strconv.Atoi(digits)
		if err != nil || n < 1 || n > 100 || strconv.Itoa(n) != digits {
			return 0, 0, errSyntax
		}
		return 0, n, nil
	}
	if len(value) < 2 {
		return 0, 0, errSyntax
	}
	shift, ok := shifts[value[len(value)-1]]
	digits := value[:len(value)-1]
	n, err := strconv.ParseInt(digits, 10, 64)
	if !ok || err != nil || n <= 0 || strconv.FormatInt(n, 10) != digits {
		return 0, 0, errSyntax
	}
	if n > math.MaxInt64>>shift {
		return 0, 0, fmt.Errorf("size %s is too large", value)
	}
	return n << shift, 0, nil
}

// Total is MemTotal of procRoot/meminfo (/proc when procRoot is ""), in bytes.
func Total(procRoot string) (int64, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, fmt.Errorf("read RAM size: %w", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "MemTotal:" && f[2] == "kB" {
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("meminfo: MemTotal %q is not a size in kB", f[1])
			}
			return n << 10, nil
		}
	}
	return 0, fmt.Errorf("meminfo: no MemTotal line")
}

// Resolve is the ceiling value means on a machine with total bytes of RAM.
func Resolve(value string, total int64) (int64, error) {
	b, p, err := Parse(value)
	if err != nil {
		return 0, err
	}
	if p != 0 {
		return total / 100 * int64(p), nil
	}
	return b, nil
}
