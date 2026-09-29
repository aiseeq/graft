package checks

import (
	"strings"
	"testing"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
)

// Credential-shaped strings are assembled at run time so this file does not
// trip the very scan it tests.
func fakeSecrets() map[string]string {
	return map[string]string{
		"pem":      "-----BEGIN " + "RSA PRIVATE KEY-----",
		"re":       `key := "re_` + strings.Repeat("a", 24) + `"`,
		"gocspx":   `secret: 'GOCSPX-` + strings.Repeat("b", 24) + `'`,
		"pgpass":   "PGPASSWORD=" + strings.Repeat("c", 24),
		"aws":      "id = AKIA" + strings.Repeat("D", 16),
		"sk_live":  `"sk_live_` + strings.Repeat("e", 24) + `"`,
		"jwt":      `"eyJ` + strings.Repeat("f", 20) + "." + strings.Repeat("g", 20) + "." + strings.Repeat("h", 20) + `"`,
		"extra":    "token_abcdefghij",
		"harmless": "re_used := compute() // AKIA",
	}
}

func parse(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("schema: 1\nversion:\n  mode: none\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func text(p, content string) StagedFile {
	return StagedFile{Path: p, Blob: gitx.Blob{Size: int64(len(content)), Head: []byte(content), Content: []byte(content)}}
}

func TestSecrets(t *testing.T) {
	cfg := parse(t, "checks:\n  secrets:\n    extra_patterns: ['token_[a-z]{10}']\n    exceptions: [{path: 'testdata/**', reason: fixtures}]\n")
	var files []StagedFile
	for name, content := range fakeSecrets() {
		files = append(files, text(name+".txt", "line one\n"+content+"\n"))
	}
	files = append(files,
		text("testdata/key.pem", fakeSecrets()["pem"]),
		StagedFile{Path: "img.png", Blob: gitx.Blob{Size: 5, Head: []byte("\x00" + fakeSecrets()["aws"]), Content: []byte("\x00" + fakeSecrets()["aws"])}},
	)
	found := map[string]Finding{}
	for _, f := range Secrets(files, cfg.Checks.Secrets) {
		found[f.Path] = f
	}
	for name := range fakeSecrets() {
		f, ok := found[name+".txt"]
		switch {
		case name == "harmless" && ok:
			t.Errorf("false positive: %v", f)
		case name != "harmless" && !ok:
			t.Errorf("%s not detected", name)
		case ok && f.Line != 2:
			t.Errorf("%s: line %d, want 2", name, f.Line)
		}
	}
	if _, ok := found["testdata/key.pem"]; ok {
		t.Error("exception ignored")
	}
	if _, ok := found["img.png"]; ok {
		t.Error("binary files are not scanned")
	}
	for _, f := range found {
		if strings.Contains(f.String(), strings.Repeat("a", 24)) {
			t.Errorf("finding echoes the secret: %s", f)
		}
	}
}

func TestSecretsUnloadedBlob(t *testing.T) {
	cfg := parse(t, "")
	got := Secrets([]StagedFile{{Path: "huge.txt", Blob: gitx.Blob{Size: maxScanBytes + 1, Head: []byte("x")}}}, cfg.Checks.Secrets)
	if len(got) != 1 || !strings.Contains(got[0].Reason, "too large") {
		t.Errorf("unscanned blob must be reported: %v", got)
	}
}

func TestLargeFiles(t *testing.T) {
	cfg := parse(t, "checks:\n  large_files:\n    max_binary_bytes: 100\n    max_text_bytes: 1000\n    exceptions: [{path: vendor/big.js, reason: vendored}]\n")
	bin := func(p string, size int64) StagedFile {
		return StagedFile{Path: p, Blob: gitx.Blob{Size: size, Head: []byte{0x7f, 'E', 'L', 'F', 0}}}
	}
	files := []StagedFile{
		bin("app", 50),        // binary, not an asset type
		bin("logo.PNG", 50),   // asset under the limit
		bin("photo.jpg", 500), // asset over the binary limit
		{Path: "data.json", Blob: gitx.Blob{Size: 2000, Head: []byte("{")}}, // text over the limit
		{Path: "ok.go", Blob: gitx.Blob{Size: 900, Head: []byte("package x")}},
		{Path: "vendor/big.js", Blob: gitx.Blob{Size: 5000, Head: []byte("x")}},
	}
	got := map[string]bool{}
	for _, f := range LargeFiles(files, cfg.Checks.LargeFiles) {
		got[f.Path] = true
	}
	want := map[string]bool{"app": true, "photo.jpg": true, "data.json": true}
	if len(got) != len(want) {
		t.Errorf("findings %v, want %v", got, want)
	}
	for p := range want {
		if !got[p] {
			t.Errorf("%s not reported", p)
		}
	}
}
