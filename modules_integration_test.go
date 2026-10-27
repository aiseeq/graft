package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// proxyModule is one version of a module the test proxy serves: its go.mod
// and the source of its one package.
type proxyModule struct {
	path, version, src string
	// latestOnly serves the version only as @latest, with an empty list:
	// a module without tags, whose latest is a pseudo-version.
	latestOnly bool
}

// goProxy writes a GOPROXY=file:// tree serving mods and returns its URL.
func goProxy(t *testing.T, mods ...proxyModule) string {
	t.Helper()
	dir := t.TempDir()
	lists := map[string][]string{}
	for _, m := range mods {
		v := filepath.Join(dir, filepath.FromSlash(m.path), "@v")
		gomod := "module " + m.path + "\n\ngo 1.21\n"
		writeFile(t, filepath.Join(v, m.version+".mod"), gomod)
		writeFile(t, filepath.Join(v, m.version+".info"), fmt.Sprintf(`{"Version":%q,"Time":"2026-01-02T00:00:00Z"}`, m.version))
		zf, err := os.Create(filepath.Join(v, m.version+".zip"))
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(zf)
		for name, content := range map[string]string{"go.mod": gomod, "lib.go": m.src} {
			w, err := zw.Create(m.path + "@" + m.version + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := zf.Close(); err != nil {
			t.Fatal(err)
		}
		if m.latestOnly {
			writeFile(t, filepath.Join(dir, filepath.FromSlash(m.path), "@latest"), fmt.Sprintf(`{"Version":%q,"Time":"2026-01-02T00:00:00Z"}`, m.version))
			if _, ok := lists[m.path]; !ok {
				lists[m.path] = nil
			}
			continue
		}
		lists[m.path] = append(lists[m.path], m.version)
	}
	for path, versions := range lists {
		list := ""
		if len(versions) > 0 {
			list = strings.Join(versions, "\n") + "\n"
		}
		writeFile(t, filepath.Join(dir, filepath.FromSlash(path), "@v", "list"), list)
	}
	return "file://" + filepath.ToSlash(dir)
}

const (
	ownLib       = "example.com/own/lib"
	ownUntagged  = "example.com/own/untagged"
	ownLocal     = "example.com/own/local"
	otherDep     = "example.com/other/dep"
	oldPseudo    = "v0.0.0-20260101000000-111111111111"
	newPseudo    = "v0.0.0-20260102000000-222222222222"
	latestConfig = "modules: {latest: [example.com/own/]}\n"
)

// modulesProxy serves an own library whose v0.2.0 adds New and drops Old, an
// own module without tags, and a foreign dependency with a newer release.
func modulesProxy(t *testing.T) string {
	return goProxy(t,
		proxyModule{path: ownLib, version: "v0.1.0", src: "package lib\n\nfunc Old() int { return 1 }\n"},
		proxyModule{path: ownLib, version: "v0.2.0", src: "package lib\n\nfunc New() int { return 2 }\n"},
		proxyModule{path: ownUntagged, version: oldPseudo, src: "package untagged\n\nconst A = 1\n", latestOnly: true},
		proxyModule{path: ownUntagged, version: newPseudo, src: "package untagged\n\nconst A, B = 1, 2\n", latestOnly: true},
		proxyModule{path: otherDep, version: "v1.0.0", src: "package dep\n\nconst X = 1\n"},
		proxyModule{path: otherDep, version: "v1.1.0", src: "package dep\n\nconst X, Y = 1, 2\n"},
	)
}

// consumerGoMod requires the own library and the foreign dependency at their
// old releases, the untagged module at its old commit, and an own module
// replaced by a directory of the repository, which no proxy has.
func consumerGoMod(module string) string {
	return "module " + module + "\n\ngo 1.21\n\nrequire (\n" +
		"\t" + ownLib + " v0.1.0\n" +
		"\t" + ownUntagged + " " + oldPseudo + "\n" +
		"\t" + ownLocal + " v0.0.0-00010101000000-000000000000\n" +
		"\t" + otherDep + " v1.0.0\n)\n\n" +
		"replace " + ownLocal + " => ./local\n"
}

// consumerMain uses lib's call, so the gate's go vet passes only at the
// release that has it.
func consumerMain(call string) string {
	return "package main\n\nimport (\n\t\"" + ownLib + "\"\n\t\"" + ownUntagged + "\"\n\t\"" + ownLocal + "\"\n\t\"" + otherDep + "\"\n)\n\n" +
		"func main() { println(lib." + call + "(), untagged.A, local.Z, dep.X) }\n"
}

// modulesRepo is a Go project with a second module in tool/, its go.sum made
// at the old versions, with userConfig as the graft user config ("" for
// none) and the environment going to the proxy at goproxy.
func modulesRepo(t *testing.T, goproxy, userConfig, call string) *fixture {
	t.Helper()
	f := vetRepo(t, "", map[string]string{
		"go.mod":         consumerGoMod("example.com/consumer"),
		"main.go":        consumerMain(call),
		"local/go.mod":   "module " + ownLocal + "\n\ngo 1.21\n",
		"local/local.go": "package local\n\nconst Z = 0\n",
		"tool/go.mod":    "module example.com/consumer/tool\n\ngo 1.21\n\nrequire " + ownLib + " v0.1.0\n",
		"tool/main.go":   "package main\n\nimport \"" + ownLib + "\"\n\nfunc main() { println(lib." + call + "()) }\n",
	})
	cfgDir := t.TempDir()
	if userConfig != "" {
		writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), userConfig)
	}
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir,
		"GOPROXY="+goproxy, "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GOFLAGS=-modcacherw",
		"GOMODCACHE="+filepath.Join(t.TempDir(), "modcache"))
	// go.sum at the old versions, as the project had it.
	for _, dir := range []string{f.work, filepath.Join(f.work, "tool")} {
		if out, code := f.exec(dir, "", "go", "mod", "download", "all"); code != 0 {
			t.Fatalf("go mod download in %s: exit %d\n%s", dir, code, out)
		}
	}
	return f
}

func requireLine(t *testing.T, gomod, module string) string {
	t.Helper()
	for line := range strings.SplitSeq(gomod, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == module || len(fields) >= 3 && fields[0] == "require" && fields[1] == module {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no require of %s in:\n%s", module, gomod)
	return ""
}

// TestCommitRaisesOwnModulesToTheirLatest: under modules.latest, graft
// commit raises every own module required by any go.mod of the repository
// to its latest version before the gate, tidies, and commits it; a foreign
// module and a replaced one stay as they are.
func TestCommitRaisesOwnModulesToTheirLatest(t *testing.T) {
	f := modulesRepo(t, modulesProxy(t), latestConfig, "New")
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code != 0 {
		t.Fatalf("commit: exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"graft: go.mod: " + ownLib + " v0.1.0 -> v0.2.0",
		"graft: go.mod: " + ownUntagged + " " + oldPseudo + " -> " + newPseudo,
		"graft: tool/go.mod: " + ownLib + " v0.1.0 -> v0.2.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	root := f.git("show", "HEAD:go.mod")
	for module, want := range map[string]string{ownLib: "v0.2.0", ownUntagged: newPseudo, otherDep: "v1.0.0", ownLocal: "v0.0.0-00010101000000-000000000000"} {
		if line := requireLine(t, root, module); !strings.HasSuffix(line, " "+want) {
			t.Errorf("committed go.mod: %q, want %s", line, want)
		}
	}
	if !strings.Contains(root, "replace "+ownLocal+" => ./local") {
		t.Errorf("the replace is gone:\n%s", root)
	}
	if line := requireLine(t, f.git("show", "HEAD:tool/go.mod"), ownLib); !strings.HasSuffix(line, " v0.2.0") {
		t.Errorf("committed tool/go.mod: %q", line)
	}
	sum := f.git("show", "HEAD:go.sum")
	if !strings.Contains(sum, ownLib+" v0.2.0 ") || strings.Contains(sum, ownLib+" v0.1.0 ") {
		t.Errorf("go.sum is not tidied to v0.2.0:\n%s", sum)
	}
	if status := f.git("status", "--porcelain"); status != "" {
		t.Errorf("left uncommitted:\n%s", status)
	}

	// At the latest already: nothing to say, nothing changes.
	f.write("main.go", consumerMain("New")+"\n// again\n")
	out = f.mustGraft("commit", "-m", "feat: y")
	if strings.Contains(out, " -> ") {
		t.Errorf("raised again:\n%s", out)
	}
}

// TestGateRaisesOwnModules: graft gate run by hand checks what a commit
// would: the own modules at their latest, left raised in the work tree.
func TestGateRaisesOwnModules(t *testing.T) {
	f := modulesRepo(t, modulesProxy(t), latestConfig, "New")
	out, code := f.graft("", "gate")
	if code != 0 {
		t.Fatalf("gate: exit %d\n%s", code, out)
	}
	if line := requireLine(t, f.read("go.mod"), ownLib); !strings.HasSuffix(line, " v0.2.0") {
		t.Errorf("go.mod after the gate: %q", line)
	}
}

// TestOwnModulesStayWithoutTheSetting: without modules.latest graft leaves
// every go.mod alone.
func TestOwnModulesStayWithoutTheSetting(t *testing.T) {
	f := modulesRepo(t, modulesProxy(t), "", "Old")
	out := f.mustGraft("commit", "-m", "feat: x")
	if strings.Contains(out, " -> ") {
		t.Errorf("raised without the setting:\n%s", out)
	}
	if line := requireLine(t, f.git("show", "HEAD:go.mod"), ownLib); !strings.HasSuffix(line, " v0.1.0") {
		t.Errorf("committed go.mod: %q", line)
	}
}

// TestBrokenLatestFailsTheCommitAndStaysRaised: a latest release the project
// does not build with fails the gate; graft does not go back to the old one,
// the work tree keeps the raised go.mod for the fix.
func TestBrokenLatestFailsTheCommitAndStaysRaised(t *testing.T) {
	f := modulesRepo(t, modulesProxy(t), latestConfig, "Old")
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code == 0 {
		t.Fatalf("a commit that does not build at the latest lib went in:\n%s", out)
	}
	if !strings.Contains(out, "graft: go.mod: "+ownLib+" v0.1.0 -> v0.2.0") || !strings.Contains(out, "gate failed") {
		t.Errorf("output:\n%s", out)
	}
	if n := f.commitCount(); n != 0 {
		t.Errorf("%d commits made", n)
	}
	if line := requireLine(t, f.read("go.mod"), ownLib); !strings.HasSuffix(line, " v0.2.0") {
		t.Errorf("go.mod went back: %q", line)
	}
}

// TestUnreachableProxyWarnsAndCommits: when the latest versions cannot be
// learned (offline), the commit goes on at the versions it has, with a
// warning naming the module.
func TestUnreachableProxyWarnsAndCommits(t *testing.T) {
	f := modulesRepo(t, modulesProxy(t), latestConfig, "Old")
	f.env = append(f.env, "GOPROXY=off")
	out, code := f.graft("", "commit", "-m", "feat: x")
	if code != 0 {
		t.Fatalf("commit offline: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "graft: warning: go.mod: ") || !strings.Contains(out, ownLib) {
		t.Errorf("no warning naming %s:\n%s", ownLib, out)
	}
	if line := requireLine(t, f.git("show", "HEAD:go.mod"), ownLib); !strings.HasSuffix(line, " v0.1.0") {
		t.Errorf("committed go.mod: %q", line)
	}
}

// TestToolsInstallTakesOwnToolsAtLatest: graft tools install installs a tool
// of an own module at its latest release, whatever version go_install names;
// the version stays the oldest the project works with.
func TestToolsInstallTakesOwnToolsAtLatest(t *testing.T) {
	const tool = "example.com/own/fake"
	proxy := goProxy(t,
		proxyModule{path: tool, version: "v0.1.0", src: "package main\n\nfunc main() {}\n"},
		proxyModule{path: tool, version: "v0.2.0", src: "package main\n\nfunc main() { println(2) }\n"},
	)
	f := newRepo(t, map[string]string{
		".graft.yaml": fileModeConfig + gateStep("ok") + "tools:\n  fake: {go_install: " + tool + "@v0.1.0}\n",
		"VERSION":     "1.2.3\n", "web/package.json": "{\n  \"version\": \"1.2.3\"\n}\n", "app.yaml": "name: app\nversion: \"1.2.3\"\n",
	}, "origin")
	bin, cfgDir := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), latestConfig+"tools: {bin_dir: "+yamlQuote(bin)+"}\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir,
		"GOPROXY="+proxy, "GOSUMDB=off", "GOPRIVATE=", "GONOPROXY=", "GONOSUMDB=", "GOFLAGS=-modcacherw",
		"GOMODCACHE="+filepath.Join(t.TempDir(), "modcache"),
		"PATH="+bin+string(os.PathListSeparator)+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, code := f.graft("", "tools", "install"); code != 0 {
		t.Fatalf("tools install: exit %d\n%s", code, out)
	}
	exe := "fake"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	out, code := f.exec(f.work, "", "go", "version", "-m", filepath.Join(bin, exe))
	if code != 0 || !strings.Contains(out, tool+"\tv0.2.0") {
		t.Errorf("installed build (exit %d):\n%s", code, out)
	}
}
