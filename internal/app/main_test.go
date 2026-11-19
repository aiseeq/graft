package app

import (
	"fmt"
	"os"
	"testing"

	"github.com/aiseeq/graft/v2/internal/gittest"
)

func TestMain(m *testing.M) {
	if err := gittest.IsolateTestProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "isolate app test git environment:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
