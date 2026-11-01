package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// leakRepo is a repository whose origin looks public to the user config,
// while pushes still go to a local bare repository. The private project
// declares SettleInvoice; the terms name a counterparty.
// extra is appended to the leaks section of the user config.
func leakRepo(t *testing.T, publicPrefix, extra string) *fixture {
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion:\n  mode: none\ngate: []\n",
		"main.go":     "package main\n\nfunc main() {}\n",
	}, "origin")
	f.git("config", "remote.origin.pushurl", f.remote("origin"))
	f.git("config", "remote.origin.url", "git@github.com:someone/tool.git")

	root := t.TempDir()
	private := filepath.Join(root, "private", "projectA")
	writeFile(t, filepath.Join(private, "go.mod"), "module example.com/projectA\n")
	writeFile(t, filepath.Join(private, "pay.go"), "package pay\n\nfunc (s *Svc) SettleInvoice() error { return nil }\nfunc (s *Svc) PolishWidget() {}\n")
	terms := filepath.Join(root, "private-terms")
	writeFile(t, terms, "counterparty: (?i)\\b(acmepay|mergealpha|mergebeta)\\b\n")
	cfgDir := filepath.Join(root, "config")
	writeUserConfig(t, cfgDir, "leaks:\n"+
		"  public_remotes: ["+publicPrefix+"]\n"+
		"  private_sources: ["+yamlQuote(private)+"]\n"+
		"  terms_file: "+yamlQuote(terms)+"\n"+
		"  allow: [{term: PolishWidget, reason: a name every project has}]\n"+extra)
	goroot := filepath.Join(root, "goroot")
	writeFile(t, filepath.Join(goroot, "VERSION"), "go1.99\n")
	writeFile(t, filepath.Join(goroot, "src", "x", "x.go"), "package x\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir,
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"), "LOCALAPPDATA="+filepath.Join(root, "cache"),
		"GOROOT="+goroot, "GOMODCACHE="+filepath.Join(root, "mod"))
	f.mustPlainCommit("feat: base")
	return f
}

func TestCommitRefusesPrivateNamesInPublicRepo(t *testing.T) {
	f := leakRepo(t, "github.com/someone/", "")
	f.write("main.go", "package main\n\n// Mirrors s.SettleInvoice of the payment service.\nfunc main() {}\n")
	out, code := f.graft("", "commit", "-m", "feat: settle like AcmePay does")
	if code == 0 {
		t.Fatalf("a private name reached a public repository:\n%s", out)
	}
	for _, want := range []string{
		"main.go:3: private name SettleInvoice (declared in projectA)",
		`commit message:1: private term (counterparty) "AcmePay"`,
		"graft:leak-ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	// Leaks have no .graft.yaml exceptions: the hint must not offer them.
	if strings.Contains(out, "checks.*.exceptions") {
		t.Errorf("leak findings offer .graft.yaml exceptions:\n%s", out)
	}

	f.write("main.go", "package main\n\n// Mirrors s.SettleInvoice of the service. graft:leak-ok the public name of the example\n// PolishWidget is allowed in the user config.\nfunc main() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: settle"); code != 0 {
		t.Errorf("marked line and allowed name: %d\n%s", code, out)
	}

	// The repository's own declaration is its vocabulary, not a leak.
	f.write("settle.go", "package main\n\nfunc SettleInvoice() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: own settle"); code != 0 {
		t.Errorf("own declaration: %d\n%s", code, out)
	}
}

func TestPrePushScansNewlyOutgoingCommit(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.git("push", "origin", "HEAD:refs/heads/main")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	local := f.git("rev-parse", "HEAD")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "refs/heads/main")
	input := "refs/heads/main " + local + " refs/heads/main " + remote + "\n"
	out, code := f.exec(f.work, input, graftBin, "hook", "pre-push", "origin", f.remote("origin"))
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") || !strings.Contains(out, "git push --no-verify") {
		t.Fatalf("pre-push exit %d:\n%s", code, out)
	}
}

func TestPrePushUsesFreshTargetRefsInsteadOfTrackingRefs(t *testing.T) {
	f := leakRepo(t, "/", "")
	base := f.git("rev-parse", "HEAD")
	f.git("push", f.remote("origin"), "HEAD:refs/heads/main")
	f.git("fetch", f.remote("origin"), "refs/heads/main:refs/remotes/origin/main")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	local := f.git("rev-parse", "HEAD")
	f.git("push", f.remote("origin"), "HEAD:refs/heads/main")
	f.git("fetch", f.remote("origin"), "refs/heads/main:refs/remotes/origin/main")
	f.gitIn(f.remote("origin"), "update-ref", "refs/heads/main", base)
	input := "refs/heads/main " + local + " refs/heads/main " + strings.Repeat("0", 40) + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", f.remote("origin"))
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") {
		t.Fatalf("stale tracking ref did not protect the target: exit %d\n%s", code, out)
	}
}

func TestPrePushScansLeakingAncestorWhenPushingCleanChild(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: private change")
	f.write("ordinary.go", "package main\n\nconst value = 1\n")
	f.git("add", "ordinary.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: ordinary child")
	local := f.git("rev-parse", "HEAD")
	input := "refs/heads/main " + local + " refs/heads/main " + strings.Repeat("0", 40) + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", f.remote("origin"))
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") {
		t.Fatalf("leaking ancestor was not scanned: exit %d\n%s", code, out)
	}
}

func setFixtureEnv(f *fixture, values ...string) {
	f.t.Helper()
	keys := make(map[string]bool, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		keys[key] = true
	}
	filtered := f.env[:0]
	for _, value := range f.env {
		key, _, _ := strings.Cut(value, "=")
		if !keys[key] {
			filtered = append(filtered, value)
		}
	}
	f.env = append(filtered, values...)
}

func installGlobalPrePush(f *fixture, capture string) {
	f.t.Helper()
	global := filepath.Join(f.base, "global.gitconfig")
	setFixtureEnv(f, "GIT_CONFIG_GLOBAL="+global)
	if out, code := f.graft("", "init", "--global"); code != 0 {
		f.t.Fatalf("graft init --global: exit %d\n%s", code, out)
	}
	bin := filepath.Join(f.base, "graft-wrapper")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		f.t.Fatal(err)
	}
	wrapper := filepath.Join(bin, "graft")
	writeFile(f.t, wrapper, "#!/bin/sh\ncat > \"$GRAFT_STDIN_CAPTURE\"\nprintf '%s\\n' \"$@\" > \"$GRAFT_ARGS_CAPTURE\"\nexec \"$GRAFT_REAL\" \"$@\" < \"$GRAFT_STDIN_CAPTURE\"\n")
	if err := os.Chmod(wrapper, 0o755); err != nil {
		f.t.Fatal(err)
	}
	path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	setFixtureEnv(f, "PATH="+path, "GRAFT_STDIN_CAPTURE="+capture,
		"GRAFT_ARGS_CAPTURE="+capture+".args", "GRAFT_REAL="+graftBin)
}

func installLocalPrePush(t *testing.T, f *fixture, capture string) {
	t.Helper()
	hooks := filepath.Join(f.base, "repo-hooks")
	hook := filepath.Join(hooks, "pre-push")
	writeFile(t, hook, "#!/bin/sh\ncat > \"$LOCAL_STDIN_CAPTURE\"\n[ \"$LOCAL_HOOK_EXIT\" != 1 ]\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	f.git("config", "core.hooksPath", hooks)
	setFixtureEnv(f, "LOCAL_STDIN_CAPTURE="+capture, "LOCAL_HOOK_EXIT=0")
}

func TestPrePushGlobalHookRunsWithRepositoryHookAndChecksTwoRefs(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.git("push", "origin", "HEAD:refs/heads/main")
	globalCapture := filepath.Join(f.base, "global-stdin")
	localCapture := filepath.Join(f.base, "local-stdin")
	installGlobalPrePush(f, globalCapture)
	installLocalPrePush(t, f, localCapture)

	base := f.git("rev-parse", "HEAD")
	f.git("switch", "-q", "-c", "leaking")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	f.git("switch", "-q", "-c", "clean", base)
	f.write("ordinary.go", "package main\n\nconst value = 1\n")
	f.git("add", "ordinary.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: ordinary")
	out, code := f.tryGit("push", "origin", "leaking:refs/heads/leaking", "clean:refs/heads/clean")
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") {
		t.Fatalf("global hook did not reject a leak in the second ref: exit %d\n%s", code, out)
	}
	globalInput, localInput := readFile(t, globalCapture), readFile(t, localCapture)
	if globalInput != localInput || strings.Count(strings.TrimSpace(globalInput), "\n") != 1 ||
		!strings.Contains(globalInput, "refs/heads/leaking") || !strings.Contains(globalInput, "refs/heads/clean") {
		t.Fatalf("hooks did not receive the same two-ref stdin:\nglobal: %q\nlocal:  %q", globalInput, localInput)
	}
	if out, code := f.tryGit("push", "origin", "clean:refs/heads/clean"); code != 0 {
		t.Fatalf("clean push: exit %d\n%s", code, out)
	}
	if strings.Count(strings.TrimSpace(readFile(t, globalCapture)), "\n") != 0 || readFile(t, globalCapture) != readFile(t, localCapture) {
		t.Fatalf("hooks did not both run for clean push: global=%q local=%q", readFile(t, globalCapture), readFile(t, localCapture))
	}
	setFixtureEnv(f, "LOCAL_HOOK_EXIT=1")
	out, code = f.tryGit("push", "origin", "clean:refs/heads/blocked")
	if code == 0 {
		t.Fatalf("repository hook refusal did not cancel push: exit %d\n%s", code, out)
	}
	if input := readFile(t, localCapture); !strings.Contains(input, "refs/heads/blocked") {
		t.Fatalf("repository hook did not receive refused push: %q", input)
	}
	if _, refCode := f.exec(f.remote("origin"), "", "git", "show-ref", "--verify", "--quiet", "refs/heads/blocked"); refCode == 0 {
		t.Fatal("repository hook refusal still created the remote branch")
	}
}

func TestPrePushGlobalHookSkipsNonPublicTarget(t *testing.T) {
	f := leakRepo(t, "github.com/someone/", "")
	capture := filepath.Join(f.base, "global-stdin")
	installGlobalPrePush(f, capture)
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	if out, code := f.tryGit("push", "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("non-public target should accept the push: exit %d\n%s", code, out)
	}
}

func TestPrePushScansCommitPublishedElsewhere(t *testing.T) {
	f := leakRepo(t, "/", "")
	other := filepath.Join(f.base, "other-public.git")
	if err := os.CopyFS(other, os.DirFS(bareTemplate)); err != nil {
		t.Fatal(err)
	}
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	local := f.git("rev-parse", "HEAD")
	f.git("push", f.remote("origin"), "HEAD:refs/heads/main")
	input := "refs/heads/main " + local + " refs/heads/main " + strings.Repeat("0", 40) + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", other)
	if code == 0 || !strings.Contains(out, "private name SettleInvoice") {
		t.Fatalf("commit present in another public repository was not scanned: exit %d\n%s", code, out)
	}
}

func TestPrePushScansMergeParentsForEmptyTarget(t *testing.T) {
	f := leakRepo(t, "/", "")
	start := f.git("branch", "--show-current")
	f.git("switch", "-q", "-c", "private-side")
	f.write("private.go", "package main\n\n// SettleInvoice\nfunc privateChange() {}\n")
	f.git("add", "private.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	f.git("switch", "-q", start)
	f.write("ordinary.go", "package main\n\nconst ordinary = 1\n")
	f.git("add", "ordinary.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: ordinary")
	f.git("merge", "--no-ff", "-m", "merge branches", "private-side")
	local := f.git("rev-parse", "HEAD")
	input := "refs/heads/main " + local + " refs/heads/main " + strings.Repeat("0", 40) + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", f.remote("origin"))
	if code == 0 || !strings.Contains(out, "private name SettleInvoice") {
		t.Fatalf("leaking merge parent was not scanned: exit %d\n%s", code, out)
	}
}

func TestPrePushUsesPushURLForVisibilityAndRange(t *testing.T) {
	f := leakRepo(t, "/", "")
	public := filepath.Join(f.base, "public-target.git")
	if err := os.CopyFS(public, os.DirFS(bareTemplate)); err != nil {
		t.Fatal(err)
	}
	var configPath string
	for _, value := range f.env {
		if after, ok := strings.CutPrefix(value, "XDG_CONFIG_HOME="); ok {
			configPath = filepath.Join(after, "graft", "config"+".yaml")
		}
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's initial public prefix is replaced with the target push path.
	old := "public_remotes: [/]"
	updated := "public_remotes: [" + yamlQuote(filepath.ToSlash(public)) + "]"
	if !strings.Contains(string(config), old) {
		t.Fatalf("fixture config lacks %q", old)
	}
	if err := os.WriteFile(configPath, []byte(strings.Replace(string(config), old, updated, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git("config", "remote.origin.url", f.remote("origin"))
	f.git("config", "--unset-all", "remote.origin.pushurl")
	f.git("config", "--add", "remote.origin.pushurl", public)
	capture := filepath.Join(f.base, "global-stdin")
	installGlobalPrePush(f, capture)
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.git("add", "main.go")
	f.git("-c", "core.hooksPath="+t.TempDir(), "commit", "--no-verify", "-m", "feat: AcmePay")
	out, code := f.tryGit("push", "origin", "HEAD:refs/heads/main")
	if code == 0 || !strings.Contains(out, "private name SettleInvoice") {
		t.Fatalf("public pushurl was not checked: exit %d\n%s", code, out)
	}
	if !strings.Contains(readFile(t, capture+".args"), public) {
		t.Fatalf("global hook did not receive push URL %q: %q", public, readFile(t, capture+".args"))
	}
}

func TestPrePushDeleteRefSkipsCommitScan(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.git("push", "origin", "HEAD:refs/heads/main")
	remote := f.gitIn(f.remote("origin"), "rev-parse", "refs/heads/main")
	input := "refs/heads/main " + strings.Repeat("0", 40) + " refs/heads/main " + remote + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", f.remote("origin"))
	if code != 0 || strings.Contains(out, "commits scanned") {
		t.Fatalf("delete-ref hook: exit %d, output %q", code, out)
	}
}

func TestPrePushExistingTargetTagSkipsCommitScan(t *testing.T) {
	f := leakRepo(t, "/", "")
	f.git("push", "origin", "HEAD:refs/heads/main")
	local := f.git("rev-parse", "HEAD")
	f.git("tag", "v1", local)
	input := "refs/tags/v1 " + local + " refs/tags/v1 " + strings.Repeat("0", 40) + "\n"
	out, code := f.graft(input, "hook", "pre-push", "origin", f.remote("origin"))
	if code != 0 || strings.Contains(out, "commits scanned") {
		t.Fatalf("published tag hook: exit %d, output %q", code, out)
	}
}

func TestCommitToPrivateRepoSkipsTheLeakCheck(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/", "")
	f.write("main.go", "package main\n\n// SettleInvoice, AcmePay\nfunc main() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: AcmePay"); code != 0 {
		t.Errorf("private repository: %d\n%s", code, out)
	}
}

// A repository under a public prefix and under a narrower private one is
// private: the narrower prefix wins.
func TestCommitToPrivateRemoteUnderPublicPrefixSkipsTheLeakCheck(t *testing.T) {
	f := leakRepo(t, "github.com/someone/", "  private_remotes: [github.com/someone/secret-tool]\n")
	f.git("config", "remote.origin.url", "git@github.com:someone/secret-tool.git")
	f.write("main.go", "package main\n\n// SettleInvoice, AcmePay\nfunc main() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: AcmePay"); code != 0 {
		t.Errorf("private repository under a public prefix: %d\n%s", code, out)
	}
}

// private_remotes leaves the other repositories under the public prefix
// checked.
func TestCommitBesidePrivateRemoteIsStillChecked(t *testing.T) {
	f := leakRepo(t, "github.com/someone/", "  private_remotes: [github.com/someone/secret-tool]\n")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: settle"); code == 0 || !strings.Contains(out, "private name SettleInvoice") {
		t.Errorf("public repository beside a private prefix: %d\n%s", code, out)
	}
}

func TestLeaksScansHistory(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/", "")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.mustGraft("commit", "-m", "feat: leak")
	out, code := f.graft("", "leaks", "HEAD")
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") || !strings.Contains(out, "2 commits scanned") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestLeaksScansMergeCommit(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/", "")
	f.write("main.go", "package main\n\nconst value = \"base\"\n")
	f.mustPlainCommit("feat: base value")
	f.git("switch", "-q", "-c", "side")
	f.write("main.go", "package main\n\nconst value = \"side\"\n")
	f.write("ordinary.go", "package main\n\nconst ordinary = \"branch change\"\n")
	f.mustPlainCommit("feat: side changes")
	f.git("switch", "-q", "-")
	f.write("main.go", "package main\n\nconst value = \"main\"\n")
	f.mustPlainCommit("feat: main change")
	if out, code := f.tryGit("merge", "--no-ff", "side"); code == 0 {
		t.Fatalf("merge should conflict, exit %d:\n%s", code, out)
	}
	f.write("main.go", "package main\n\nconst value = \"MergeAlpha\"\n")
	f.git("add", "main.go")
	f.mustPlainCommit("MergeBeta merge resolution")

	out, code := f.graft("", "leaks", "--all")
	if code == 0 || !strings.Contains(out, "main.go:3: private term (counterparty) \"MergeAlpha\"") ||
		!strings.Contains(out, "commit message:1: private term (counterparty) \"MergeBeta\"") {
		t.Errorf("merge findings, exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "ordinary.go") || strings.Contains(out, "branch change") {
		t.Errorf("ordinary second-parent change was reported:\n%s", out)
	}

	clean := leakRepo(t, "github.com/somebody-else/", "")
	clean.write("base.go", "package main\n\nconst base = 1\n")
	clean.mustPlainCommit("feat: base")
	clean.git("switch", "-q", "-c", "side")
	clean.write("side.go", "package main\n\nconst side = 1\n")
	clean.mustPlainCommit("feat: side")
	clean.git("switch", "-q", "-")
	clean.write("main.go", "package main\n\nconst mainValue = 1\n")
	clean.mustPlainCommit("feat: main")
	clean.run(clean.work, "git", "merge", "--no-ff", "-m", "merge clean branches", "side")
	if out, code := clean.graft("", "leaks", "--all"); code != 0 {
		t.Errorf("clean merge should have no findings, exit %d:\n%s", code, out)
	}

	// git's remerge diff skips an octopus merge: graft refuses it by name.
	octo := leakRepo(t, "github.com/somebody-else/", "")
	octo.write("base.go", "package main\n\nconst base = 1\n")
	octo.mustPlainCommit("feat: base")
	start := octo.git("branch", "--show-current")
	for _, b := range []string{"x", "y"} {
		octo.git("switch", "-q", "-c", b, start)
		octo.write(b+".go", "package main\n\nconst "+b+" = 1\n")
		octo.mustPlainCommit("feat: " + b)
	}
	octo.git("switch", "-q", start)
	octo.run(octo.work, "git", "merge", "--no-ff", "-m", "octopus", "x", "y")
	head := octo.git("rev-parse", "HEAD")
	if out, code := octo.graft("", "leaks", "--all"); code == 0 || !strings.Contains(out, head) || !strings.Contains(out, "3 parents") {
		t.Errorf("octopus merge should be refused by name, exit %d:\n%s", code, out)
	}
}

// TestLeaksNamesAnyFileName: git quotes a path with a quote, and with
// core.quotePath=false leaves a byte outside UTF-8 raw inside the quotes,
// which Go's unquoting would turn into U+FFFD. The name goes straight into the
// index: not every file system takes it.
func TestLeaksNamesAnyFileName(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/", "")
	f.git("config", "core.quotePath", "false")
	f.write("blob", "AcmePay\n")
	blob := f.git("hash-object", "-w", "blob")
	name := "bad\xff\"q.txt"
	f.git("update-index", "--add", "--cacheinfo", "100644,"+blob+","+name)
	f.run(f.work, "git", "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "feat: odd name")
	out, code := f.graft("", "leaks", "HEAD")
	if code == 0 || !strings.Contains(out, "  "+name+":1: private term (counterparty) \"AcmePay\"") {
		t.Errorf("exit %d:\n%q", code, out)
	}
}

// TestLeaksTakesAnyRevisions: every argument goes to git rev-list, so --all
// reaches a commit only a tag holds.
func TestLeaksTakesAnyRevisions(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/", "")
	f.git("switch", "-q", "-c", "side")
	f.write("side.go", "package main\n\n// SettleInvoice\n")
	f.mustPlainCommit("feat: side")
	f.git("tag", "-a", "v9.9.9", "-m", "side tag")
	f.git("switch", "-q", "-")
	f.git("branch", "-D", "side")

	out, code := f.graft("", "leaks", "HEAD")
	if code != 0 {
		t.Fatalf("HEAD alone holds no leak, exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "leaks", "--all")
	if code == 0 || !strings.Contains(out, "side.go:3: private name SettleInvoice") {
		t.Errorf("--all must reach the tag-only commit, exit %d:\n%s", code, out)
	}
	out, code = f.graft("", "leaks", "v9.9.9", "^HEAD")
	if code == 0 || !strings.Contains(out, "side.go:3") || !strings.Contains(out, "1 commits scanned") {
		t.Errorf("several revisions go to rev-list together, exit %d:\n%s", code, out)
	}
	if out, code := f.graft("", "leaks"); code == 0 {
		t.Errorf("no revision must stay a usage error:\n%s", out)
	}
	if out, code := f.graft("", "leaks", "--bogus-flag"); code == 0 {
		t.Errorf("an option rev-list rejects must fail:\n%s", out)
	}
}
