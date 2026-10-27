package gomods

import (
	"testing"

	"github.com/aiseeq/graft/v2/internal/userconfig"
)

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
