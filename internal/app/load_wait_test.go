package app

import (
	"bytes"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestLoadWaitStartWarning(t *testing.T) {
	var out bytes.Buffer
	warnLoadWaitThenStart(&out, config.LoadWait{ThenStartExplicit: true})
	const want = "graft: warning: deploy.load_wait.then: start no longer starts over the limit (user rule); treated as fail\n"
	if out.String() != want {
		t.Fatalf("warning = %q; want %q", out.String(), want)
	}
	out.Reset()
	warnLoadWaitThenStart(&out, config.LoadWait{})
	if out.Len() != 0 {
		t.Fatalf("default load_wait emitted warning %q", out.String())
	}
}
