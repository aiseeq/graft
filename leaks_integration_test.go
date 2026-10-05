package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// leakRepo is a repository whose origin looks public to the user config,
// while pushes still go to a local bare repository. The private project
// declares SettleInvoice; the terms name a counterparty.
func leakRepo(t *testing.T, publicPrefix string) *fixture {
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
	writeFile(t, terms, "counterparty: (?i)\\bacmepay\\b\n")
	cfgDir := filepath.Join(root, "config")
	writeFile(t, filepath.Join(cfgDir, "graft", "config.yaml"), "leaks:\n"+
		"  public_remotes: ["+publicPrefix+"]\n"+
		"  private_sources: ["+yamlQuote(private)+"]\n"+
		"  terms_file: "+yamlQuote(terms)+"\n"+
		"  allow: [{term: PolishWidget, reason: a name every project has}]\n")
	goroot := filepath.Join(root, "goroot")
	writeFile(t, filepath.Join(goroot, "VERSION"), "go1.99\n")
	writeFile(t, filepath.Join(goroot, "src", "x", "x.go"), "package x\n")
	f.env = append(f.env, "XDG_CONFIG_HOME="+cfgDir, "APPDATA="+cfgDir,
		"XDG_CACHE_HOME="+filepath.Join(root, "cache"), "LOCALAPPDATA="+filepath.Join(root, "cache"),
		"GOROOT="+goroot, "GOMODCACHE="+filepath.Join(root, "mod"))
	f.mustGraft("commit", "-m", "feat: base")
	return f
}

func TestCommitRefusesPrivateNamesInPublicRepo(t *testing.T) {
	f := leakRepo(t, "github.com/someone/")
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

func TestCommitToPrivateRepoSkipsTheLeakCheck(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/")
	f.write("main.go", "package main\n\n// SettleInvoice, AcmePay\nfunc main() {}\n")
	if out, code := f.graft("", "commit", "-m", "feat: AcmePay"); code != 0 {
		t.Errorf("private repository: %d\n%s", code, out)
	}
}

func TestLeaksScansHistory(t *testing.T) {
	f := leakRepo(t, "github.com/somebody-else/")
	f.write("main.go", "package main\n\n// SettleInvoice\nfunc main() {}\n")
	f.mustGraft("commit", "-m", "feat: leak")
	out, code := f.graft("", "leaks", "HEAD")
	if code == 0 || !strings.Contains(out, "main.go:3: private name SettleInvoice") || !strings.Contains(out, "2 commits scanned") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
