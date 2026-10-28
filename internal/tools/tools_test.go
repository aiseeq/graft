package tools

import (
	"runtime/debug"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
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
	unpinned, latest := &config.Tool{GoInstall: pkg}, &config.Tool{GoInstall: pkg + "@latest"}
	tagged := &config.Tool{GoInstall: pkg + "@v1.3.0", Tags: []string{"postgres", "netgo"}}
	cases := []struct {
		name string
		tool *config.Tool
		bi   *debug.BuildInfo
		want string
	}{
		{"match", pinned, info(pkg, "v1.3.0", ""), ""},
		{"tags not pinned are not checked", pinned, info(pkg, "v1.3.0", "x"), ""},
		{"newer release", pinned, info(pkg, "v1.4.2", ""), ""},
		{"newer pre-release", pinned, info(pkg, "v1.4.0-rc1", ""), "built from example.com/tool/cmd/tool@v1.4.0-rc1, pinned v1.3.0"},
		{"older version", pinned, info(pkg, "v1.2.0", ""), "built from example.com/tool/cmd/tool@v1.2.0, pinned v1.3.0"},
		{"local build", pinned, info(pkg, "(devel)", ""), "built from example.com/tool/cmd/tool@(devel), pinned v1.3.0"},
		{"dirty build", pinned, info(pkg, "v1.3.0+dirty", ""), "built from example.com/tool/cmd/tool@v1.3.0+dirty, pinned v1.3.0"},
		{"other package", pinned, info("example.com/other/cmd/tool", "v1.3.0", ""), "built from example.com/other/cmd/tool@v1.3.0, go_install example.com/tool/cmd/tool@v1.3.0"},
		{"unpinned: any build", unpinned, info(pkg, "(devel)", ""), ""},
		{"@latest: any build", latest, info(pkg, "v0.1.0", ""), ""},
		{"unpinned: other package", unpinned, info("example.com/other/cmd/tool", "v1.3.0", ""), "built from example.com/other/cmd/tool@v1.3.0, go_install example.com/tool/cmd/tool"},
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

func TestLatestProblem(t *testing.T) {
	cases := []struct {
		name, module, built, latest, want string
	}{
		{"older release", "example.com/tool", "v0.1.0", "v0.2.0", "built from example.com/tool@v0.1.0, latest v0.2.0"},
		{"same release", "example.com/tool", "v0.2.0", "v0.2.0", ""},
		{"newer clean release", "example.com/tool", "v0.3.0", "v0.2.0", ""},
		{"pseudo-version behind release", "example.com/tool", "v0.0.0-20260101000000-111111111111", "v0.2.0", "built from example.com/tool@v0.0.0-20260101000000-111111111111, latest v0.2.0"},
		{"pre-release", "example.com/tool", "v0.2.0-rc1", "v0.2.0", "built from example.com/tool@v0.2.0-rc1, latest v0.2.0"},
		{"pseudo-version past the release", "example.com/tool", "v0.2.1-0.20261028021119-9e7c17d060cd", "v0.2.0", ""},
		{"pseudo-version of an untagged module behind the latest", "example.com/tool", "v0.0.0-20260101000000-111111111111", "v0.0.0-20260201000000-222222222222", "built from example.com/tool@v0.0.0-20260101000000-111111111111, latest v0.0.0-20260201000000-222222222222"},
		{"local build", "example.com/tool", "(devel)", "v0.2.0", ""},
		{"dirty build", "example.com/tool", "v0.2.0+dirty", "v0.3.0", ""},
	}
	for _, c := range cases {
		got := latestProblem(c.module, c.built, c.latest)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInstallArg(t *testing.T) {
	for goInstall, want := range map[string]string{
		"example.com/tool":        "example.com/tool@latest",
		"example.com/tool@latest": "example.com/tool@latest",
		"example.com/tool@v1.3.0": "example.com/tool@v1.3.0",
	} {
		if got := (&config.Tool{GoInstall: goInstall}).InstallArg(); got != want {
			t.Errorf("%s: %s, want %s", goInstall, got, want)
		}
	}
}
