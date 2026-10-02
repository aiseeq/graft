package tools

import (
	"runtime/debug"
	"testing"

	"github.com/aiseeq/graft/internal/config"
)

func TestBuildProblem(t *testing.T) {
	info := func(path, version, tags string) *debug.BuildInfo {
		bi := &debug.BuildInfo{Path: path, Main: debug.Module{Path: "example.com/tool", Version: version}}
		if tags != "" {
			bi.Settings = []debug.BuildSetting{{Key: "-tags", Value: tags}}
		}
		return bi
	}
	const pkg = "example.com/tool/cmd/tool"
	pinned := &config.Tool{GoInstall: pkg + "@v1.3.0"}
	tagged := &config.Tool{GoInstall: pkg + "@v1.3.0", Tags: []string{"postgres", "netgo"}}
	cases := []struct {
		name string
		tool *config.Tool
		bi   *debug.BuildInfo
		want string
	}{
		{"match", pinned, info(pkg, "v1.3.0", ""), ""},
		{"tags not pinned are not checked", pinned, info(pkg, "v1.3.0", "x"), ""},
		{"older version", pinned, info(pkg, "v1.2.0", ""), "built from example.com/tool/cmd/tool@v1.2.0, pinned v1.3.0"},
		{"local build", pinned, info(pkg, "(devel)", ""), "built from example.com/tool/cmd/tool@(devel), pinned v1.3.0"},
		{"dirty build", pinned, info(pkg, "v1.3.0+dirty", ""), "built from example.com/tool/cmd/tool@v1.3.0+dirty, pinned v1.3.0"},
		{"other package", pinned, info("example.com/other/cmd/tool", "v1.3.0", ""), "built from example.com/other/cmd/tool@v1.3.0, pinned example.com/tool/cmd/tool@v1.3.0"},
		{"tags in any order", tagged, info(pkg, "v1.3.0", "netgo,postgres"), ""},
		{"no tags", tagged, info(pkg, "v1.3.0", ""), "built without tags netgo,postgres"},
		{"other tags", tagged, info(pkg, "v1.3.0", "postgres"), "built with tags postgres, pinned netgo,postgres"},
		{"extra tag", tagged, info(pkg, "v1.3.0", "netgo,postgres,debug"), "built with tags debug,netgo,postgres, pinned netgo,postgres"},
	}
	for _, c := range cases {
		if got := buildProblem(c.bi, c.tool); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
