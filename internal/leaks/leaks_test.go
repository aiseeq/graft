package leaks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testRules(t *testing.T) *Rules {
	t.Helper()
	src := filepath.Join(t.TempDir(), "projectA")
	write(t, filepath.Join(src, "go.mod"), "module example.com/projectA\n")
	write(t, filepath.Join(src, "repo.go"), `package repo

func (r *Repo) SettleInvoice(id string) error { return nil }
func short() {}
func lowercaseonly() {}
type (
	LedgerEntryKind int
	x int
)
type QuoteBook struct{}
`)
	write(t, filepath.Join(src, "repo_test.go"), "package repo\n\nfunc TestRepo_RebookTransfer_Keeps(t *testing.T) {}\n")
	write(t, filepath.Join(src, "vendor", "lib", "lib.go"), "package lib\n\nfunc VendoredHelper() {}\n")
	terms := filepath.Join(t.TempDir(), "terms")
	write(t, terms, "# comment\nwork item key: \\bPROJ-[0-9]+\\b\ncounterparty: (?i)\\bacmepay\\b\n")
	r, err := Load(Options{Sources: []string{src}, TermsFile: terms, MinNameLength: 9, Allow: map[string]string{"PROJ-1": "the example key of the docs"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadCollectsDistinctiveDeclarations(t *testing.T) {
	r := testRules(t)
	var names []string
	for n := range r.names {
		names = append(names, n)
	}
	slices.Sort(names)
	// RebookTransfer survives only in a test name; the test's own name and
	// vendored code are not the project's.
	want := []string{"LedgerEntryKind", "QuoteBook", "RebookTransfer", "SettleInvoice"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if !slices.Equal(r.Modules(), []string{"example.com/projectA"}) {
		t.Errorf("modules = %v", r.Modules())
	}
}

func TestScanLine(t *testing.T) {
	r := testRules(t)
	cases := map[string][]string{
		"x := repo.SettleInvoice(id) // see PROJ-12":                   {"private name SettleInvoice (declared in projectA)", `private term (work item key) "PROJ-12"`},
		"func TestX_RebookTransfer(t *testing.T)":                      {"private name RebookTransfer (declared in projectA)"},
		"paid through AcmePay yesterday":                               {`private term (counterparty) "AcmePay"`},
		"settleInvoice and SettleInvoices differ, PROJ-1 ok":           nil,
		"repo.SettleInvoice(id) // graft:leak-ok: the public API name": nil,
	}
	for line, want := range cases {
		hits, err := r.ScanLine(line)
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		var got []string
		for _, h := range hits {
			got = append(got, h.Reason)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
	if _, err := r.ScanLine("repo.SettleInvoice() // graft:leak-ok"); err == nil || !strings.Contains(err.Error(), "needs a reason") {
		t.Errorf("marker without a reason: %v", err)
	}
}

func TestDropCommon(t *testing.T) {
	r := testRules(t)
	r.DropCommon(&Corpus{words: []string{"LedgerEntryKind", "QuoteBook"}})
	if _, ok := r.names["QuoteBook"]; ok || r.Names() != 2 {
		t.Errorf("names after dropping the common ones: %v", r.names)
	}
}

func TestReadTermsRejectsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terms")
	write(t, path, "no separator here\n")
	if _, err := ReadTerms(path); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Errorf("malformed line: %v", err)
	}
	write(t, path, "kind: (unclosed\n")
	if _, err := ReadTerms(path); err == nil {
		t.Error("a bad regexp passed")
	}
}

func TestParseDiff(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -3,0 +4,2 @@ func x()",
		"+first",
		"+++ looks like a header",
		"@@ -9 +11 @@",
		"-old",
		"+replaced",
		`\ No newline at end of file`,
		"diff --git a/gone.txt b/gone.txt",
		"--- a/gone.txt",
		"+++ /dev/null",
		"Binary files a/img.png and b/img.png differ",
		"",
	}, "\n")
	lines, err := ParseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{"a.go", 4, "first"}, {"a.go", 5, "++ looks like a header"}, {"a.go", 11, "replaced"}}
	if !slices.Equal(lines, want) {
		t.Errorf("lines = %v, want %v", lines, want)
	}
	if _, err := ParseDiff("+++ b/a.go\n@@ -1 +1,2 @@\n+only one\n"); err == nil {
		t.Error("a truncated hunk passed")
	}
}

func TestCorpusExcludesOwnModules(t *testing.T) {
	root := t.TempDir()
	goroot, mod, cache := filepath.Join(root, "go"), filepath.Join(root, "mod"), filepath.Join(root, "cache")
	write(t, filepath.Join(goroot, "VERSION"), "go1.99\ntime x\n")
	write(t, filepath.Join(goroot, "src", "fmt", "print.go"), "package fmt\n\nfunc StdlibPrinter() {}\n")
	write(t, filepath.Join(mod, "example.com", "lib@v1.0.0", "lib.go"), "package lib\n\nfunc CommonHelper() { snake_CaseWord() }\n")
	write(t, filepath.Join(mod, "example.com", "!someone", "own@v1.0.0", "own.go"), "package own\n\nfunc LeakedName() {}\n")
	write(t, filepath.Join(mod, "cache", "download", "x.go"), "package x\n\nfunc CachedOnly() {}\n")
	t.Setenv("GOROOT", goroot)
	t.Setenv("GOMODCACHE", mod)
	var log strings.Builder
	c, err := LoadCorpus(cache, []string{"example.com/Someone/"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]bool{"StdlibPrinter": true, "CommonHelper": true, "CaseWord": true, "LeakedName": false, "CachedOnly": false} {
		if c.Has(word) != want {
			t.Errorf("Has(%s) = %v, want %v", word, !want, want)
		}
	}
	if !strings.Contains(log.String(), "2 public Go modules") {
		t.Errorf("log: %q", log.String())
	}
	// A second load reads only what is new.
	write(t, filepath.Join(mod, "example.com", "next@v1.0.0", "next.go"), "package next\n\nfunc NewcomerName() {}\n")
	log.Reset()
	if c, err = LoadCorpus(cache, []string{"example.com/Someone/"}, &log); err != nil {
		t.Fatal(err)
	}
	if !c.Has("NewcomerName") || !c.Has("CommonHelper") || !strings.Contains(log.String(), "1 public Go modules") {
		t.Errorf("incremental load: %q", log.String())
	}
}
