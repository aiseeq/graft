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

func TestShellArgs(t *testing.T) {
	cases := []struct {
		line string
		args []string
		want string
	}{
		{"logs {args}", []string{"a b", "c"}, "logs 'a b' c"},
		{"logs {args}", nil, "logs "},
		{"{args} x", []string{"a$b"}, "'a$b' x"},
		{"docker logs app-{args} -n 5", []string{"blue"}, "docker logs app-blue -n 5"},
		{"x --name={args}", []string{"it's"}, `x --name='it'\''s'`},
	}
	for _, c := range cases {
		got, err := ShellArgs(c.line, c.args, nil)
		if err != nil || got != c.want {
			t.Errorf("ShellArgs(%q, %q) = %q, %v; want %q", c.line, c.args, got, err, c.want)
		}
	}
	if _, err := ShellArgs("app-{args}", []string{"a", "b"}, nil); err == nil {
		t.Error("two arguments inside a word must fail")
	}
	// The fill function sees the configured text, never the arguments.
	got, err := ShellArgs("tail -n {lines} {args}", []string{"{lines}"}, func(s string) (string, error) {
		return strings.ReplaceAll(s, "{lines}", "5"), nil
	})
	if err != nil || got != "tail -n 5 '{lines}'" {
		t.Errorf("fill: %q, %v", got, err)
	}
}

func TestExpandArgvKeepsArgumentsVerbatim(t *testing.T) {
	got, err := ExpandArgv([]string{"tar", "{args}", "--to={args}"}, []string{"a$$b${X}"}, nil)
	if err != nil || strings.Join(got, "|") != "tar|a$$b${X}|--to=a$$b${X}" {
		t.Errorf("got %q, %v", got, err)
	}
}
