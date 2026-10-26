package tasks

import (
	"slices"
	"testing"
)

func TestGoPackages(t *testing.T) {
	for _, c := range []struct {
		words []string
		want  []int
	}{
		{[]string{"go", "test", "-p", "4", "-json", "{args}", "./..."}, []int{6}},
		{[]string{"-v", "./cmd/x/", "-run=TestA", "."}, []int{1, 3}},
		{[]string{"-run", "./x", "-timeout", "20m"}, nil},
		{[]string{"-race", "./a", "-args", "./b"}, []int{1}},
		{[]string{"-count=1", "example.com/m/p"}, []int{1}},
	} {
		if got := goPackages(c.words); !slices.Equal(got, c.want) {
			t.Errorf("goPackages(%q) = %v, want %v", c.words, got, c.want)
		}
	}
}

func TestOwnPackagesKeepsOtherSteps(t *testing.T) {
	for _, argv := range [][]string{
		{"env", "A=1", "go", "test", "{args}", "./..."},
		{"go", "test", "./..."},
		{"go", "vet", "{args}", "./..."},
	} {
		if got, dropped := ownPackages(argv, []string{"./cmd/x"}); !slices.Equal(got, argv) || dropped != nil {
			t.Errorf("ownPackages(%q) = %q, dropped %q", argv, got, dropped)
		}
	}
}

func TestTrimPaths(t *testing.T) {
	for _, c := range []struct {
		argv, env, want []string
	}{
		{[]string{"go", "test", "./..."}, nil, []string{"go", "test", "-trimpath", "./..."}},
		{[]string{"go", "vet", "./..."}, nil, []string{"go", "vet", "./..."}},
		{[]string{"go", "build", "--trimpath", "."}, nil, []string{"go", "build", "--trimpath", "."}},
		{[]string{"go", "run", "."}, []string{"GOFLAGS=-p=2 -trimpath=false"}, []string{"go", "run", "."}},
		{[]string{"go", "run", "."}, []string{"GOFLAGS=-trimpath", "GOFLAGS=-p=2"}, []string{"go", "run", "-trimpath", "."}},
		{[]string{"go", "test", "-args", "-trimpath"}, nil, []string{"go", "test", "-trimpath", "-args", "-trimpath"}},
		{[]string{"gotest", "test"}, nil, []string{"gotest", "test"}},
	} {
		if got := trimPaths(c.argv, c.env); !slices.Equal(got, c.want) {
			t.Errorf("trimPaths(%q, %q) = %q, want %q", c.argv, c.env, got, c.want)
		}
	}
}
