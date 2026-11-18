package gomods

import (
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

func TestEnvWithNameCaseByOS(t *testing.T) {
	env := []string{"GOWORK=old", "gowork=lower", "PATH=/bin"}
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"linux", []string{"gowork=lower", "PATH=/bin", "GOWORK=off"}},
		{"windows", []string{"PATH=/bin", "GOWORK=off"}},
	} {
		if got := envWithForOS(env, "GOWORK", "off", tc.goos); !slices.Equal(got, tc.want) {
			t.Errorf("envWithForOS on %s = %#v, want %#v", tc.goos, got, tc.want)
		}
	}
}

func TestSelectCandidates(t *testing.T) {
	const gomod = `module example.com/own/app

go 1.21

require (
	example.com/own/app v1.0.0
	example.com/own/lib v1.2.0
	example.com/own/local v1.0.0
	example.com/own/replaced v1.0.0
	example.com/ownish/lib v1.0.0
	example.com/other/lib v1.0.0
)

replace example.com/own/local v1.0.0 => ../local
replace example.com/own/replaced => ../replaced
`
	got, err := selectCandidates([]byte(gomod), userconfig.Modules{Latest: []string{"example.com/own"}}.IsLatest)
	if err != nil {
		t.Fatal(err)
	}
	want := []candidate{{"example.com/own/lib", "v1.2.0"}}
	if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) {
		t.Fatalf("candidates: %#v, want %#v", got, want)
	}
}

func TestFormatRaisedModules(t *testing.T) {
	got := formatRaisedModules(
		[]candidate{{path: "example.com/akit", version: "v1.0.0"}, {path: "example.com/graft/v2", version: "v2.0.0"}},
		map[string]LatestModule{"example.com/akit": {Version: "v1.1.0"}, "example.com/graft/v2": {Version: "v2.1.0"}},
	)
	const want = "example.com/akit v1.0.0 -> v1.1.0, example.com/graft/v2 v2.0.0 -> v2.1.0"
	if got != want || strings.Contains(got, "\n") {
		t.Fatalf("formatRaisedModules = %q, want one line %q", got, want)
	}
}
