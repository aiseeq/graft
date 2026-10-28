package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestToolsUnpinnedFollowLatest: a go_install tool without a version is
// present only as the module's latest release: an older build on PATH is
// reported against the latest, with and without check, and graft tools
// install replaces it; when the latest cannot be learned, the build on PATH
// passes with a warning that says so.
func TestToolsUnpinnedFollowLatest(t *testing.T) {
	const (
		plain   = "example.com/other/fake"
		checked = "example.com/other/checked"
	)
	proxy := goProxy(t,
		proxyModule{path: plain, version: "v0.1.0", src: "package main\n\nfunc main() {}\n"},
		proxyModule{path: plain, version: "v0.2.0", src: "package main\n\nfunc main() { println(2) }\n"},
		proxyModule{path: checked, version: "v0.1.0", src: "package main\n\nfunc main() { println(\"checked 1\") }\n"},
		proxyModule{path: checked, version: "v0.2.0", src: "package main\n\nfunc main() { println(\"checked 2\") }\n"},
	)
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\ngate:\n" + gateStep("ok") +
			"tools:\n  fake: {go_install: " + plain + "}\n  checked: {go_install: " + checked + ", check: [checked], expect: checked}\n",
	})
	bin, cfgDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "tools: {bin_dir: "+yamlQuote(bin)+"}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir,
		"GOPROXY="+proxy, "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GOFLAGS=-modcacherw",
		"GOMODCACHE="+filepath.Join(t.TempDir(), "modcache"), "GOBIN="+bin,
		"PATH="+bin+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, m := range []string{plain, checked} {
		if out, code := f.exec(f.work, "", "go", "install", m+"@v0.1.0"); code != 0 {
			t.Fatalf("go install %s@v0.1.0: exit %d\n%s", m, code, out)
		}
	}

	out, code := f.graft("", "tools")
	if code == 0 {
		t.Errorf("graft tools passed the v0.1.0 builds with v0.2.0 out:\n%s", out)
	}
	for _, want := range []string{"fake: built from " + plain + "@v0.1.0, latest v0.2.0", "checked: built from " + checked + "@v0.1.0, latest v0.2.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("graft tools lacks %q:\n%s", want, out)
		}
	}

	if out, code := f.graft("", "tools", "install"); code != 0 {
		t.Fatalf("tools install: exit %d\n%s", code, out)
	}
	for _, name := range []string{"fake", "checked"} {
		exe := name
		if runtime.GOOS == "windows" {
			exe += ".exe"
		}
		out, code := f.exec(f.work, "", "go", "version", "-m", filepath.Join(bin, exe))
		if code != 0 || !strings.Contains(out, "\tv0.2.0") {
			t.Errorf("%s after tools install (exit %d):\n%s", name, code, out)
		}
	}
	if out, code := f.graft("", "tools"); code != 0 || !strings.Contains(out, "fake: ok") || !strings.Contains(out, "checked: ok") {
		t.Errorf("graft tools on the latest builds: exit %d\n%s", code, out)
	}

	// Offline the build on PATH passes, and graft says the latest is unknown.
	f.env = append(f.env, "GOPROXY=off")
	out, code = f.graft("", "tools")
	if code != 0 || !strings.Contains(out, "fake: ok") {
		t.Errorf("graft tools offline: exit %d\n%s", code, out)
	}
	for _, want := range []string{"latest version of " + plain + " is unknown", "latest version of " + checked + " is unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("graft tools offline lacks %q:\n%s", want, out)
		}
	}
}
