package hooks

import (
	"testing"

	"github.com/aiseeq/graft/v2/internal/testinputs"
)

// The processes this package's tests run read the environment, and go test
// keys a cached result on it only when the test reads it too.
func TestChildInputsKeyTheCache(t *testing.T) { testinputs.Env() }
