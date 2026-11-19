package gittest

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if err := IsolateTestProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "isolate gittest test git environment:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
