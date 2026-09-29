package envs

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/dotenv"
)

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      "plain",
		"":           "''",
		"a b":        "'a b'",
		"it's":       `'it'\''s'`,
		"$HOME":      "'$HOME'",
		"/opt/app/x": "/opt/app/x",
		"k=v":        "k=v",
		"a;rm -rf /": "'a;rm -rf /'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestFill(t *testing.T) {
	got, err := Fill("/api/items/{id}/status?x={id}", map[string]string{"id": "a b/c"}, url.PathEscape)
	if err != nil || got != "/api/items/a%20b%2Fc/status?x=a%20b%2Fc" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := Fill("{method} {pth}", map[string]string{"method": "GET"}, Quote); err == nil || !strings.Contains(err.Error(), "{pth}") {
		t.Errorf("typo not reported: %v", err)
	}
	got, err = Fill("X={body} bash -s", map[string]string{"body": "a'b"}, Quote)
	if err != nil || got != `X='a'\''b' bash -s` {
		t.Errorf("got %q", got)
	}
}

func TestCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("GREETING=hello there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := &Env{Name: "local", Root: root, Lookup: dotenv.NewLookup(filepath.Join(root, ".env"))}
	cmd, err := local.Command(context.Background(), config.EnvCommand{Argv: []string{"echo", "${GREETING}"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil || string(out) != "hello there\n" {
		t.Errorf("argv: %q, %v", out, err)
	}
	cmd, _ = local.Command(context.Background(), config.EnvCommand{Shell: "echo $((1+2)) | tr 3 x"})
	if out, err := cmd.Output(); err != nil || string(out) != "x\n" {
		t.Errorf("shell: %q, %v", out, err)
	}

	remote := &Env{Name: "prod", Config: config.Env{SSH: &config.SSH{Host: "10.0.0.2", User: "deploy", Port: 2222, Key: "/k", Options: []string{"ConnectTimeout=10"}}}}
	cmd, err = remote.Command(context.Background(), config.EnvCommand{Argv: []string{"cat", "/opt/app/my file"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-p", "2222", "-i", "/k", "-o", "ConnectTimeout=10", "deploy@10.0.0.2", "cat '/opt/app/my file'"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("ssh args = %q", cmd.Args)
	}
}
