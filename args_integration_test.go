package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOptionalKeysAndSets(t *testing.T) {
	f := tasksRepo(t, "", map[string]string{".env": "A=1\nC=3\n"})
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`dotenv_sets:
  db: [A, 'B?']
tasks:
  env:
    dotenv_sets: [db]
    dotenv: [C]
    run: [`+helperStep("env", out, "A", "B", "C")+`]
`)
	f.mustGraft("env")
	f.env = []string{"B=2"}
	f.mustGraft("env")
	if got := strings.Join(f.lines("out.txt"), ","); got != "A=1,B unset,C=3,A=1,B=2,C=3" {
		t.Errorf("env = %s", got)
	}
}

func TestMissingKeysStopTheTaskBeforeItsDeps(t *testing.T) {
	f := tasksRepo(t, "", map[string]string{".env": "A=1\n"})
	built := f.path("built.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  build:
    dotenv: [BUILD_KEY]
    run: [`+helperStep("append", built, "built")+`]
  ship:
    deps: [build]
    dotenv: [A, DEPLOY_KEY]
    env: {URL: 'http://${HOST}'}
    run: [`+helperStep("append", built, "${TOKEN}")+`]
`)
	out, code := f.graft("", "ship")
	if code != 1 || !strings.Contains(out, "nothing was run") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, key := range []string{"build: BUILD_KEY", "ship: DEPLOY_KEY", "ship: HOST", "ship: TOKEN"} {
		if !strings.Contains(out, key+" is not set") {
			t.Errorf("missing %q in:\n%s", key, out)
		}
	}
	if _, err := os.Stat(built); !os.IsNotExist(err) {
		t.Errorf("a step ran: %v", err)
	}
}

func TestArgumentsAreVerbatim(t *testing.T) {
	f := tasksRepo(t, "", map[string]string{".env": "DIR=/srv\n"})
	out := f.path("out.txt")
	f.write(".graft.yaml", f.read(".graft.yaml")+`tasks:
  all:
    run: [`+helperStep("append", out, "${DIR}", "{args}")+`]
  one:
    desc: restore a backup
    args: required
    usage: <backup.tar.gz>
    run: [`+helperStep("append", out, "--from=${DIR}/{args}")+`]
`)
	f.mustGraft("all", "--", "a$$b", "x${NOPE}")
	f.mustGraft("one", "--", "c$$d${NOPE}")
	if got := f.lines("out.txt"); len(got) != 2 || got[0] != "/srv a$$b x${NOPE}" || got[1] != "--from=/srv/c$$d${NOPE}" {
		t.Errorf("lines = %q", got)
	}
	if res, code := f.graft("", "one", "--", "a", "b"); code != 1 || !strings.Contains(res, "takes exactly one argument, got 2") {
		t.Errorf("two args in a word: exit %d\n%s", code, res)
	}
	if res, code := f.graft("", "one"); code != 1 || !strings.Contains(res, "task one needs arguments: graft one -- <backup.tar.gz>") {
		t.Errorf("required args: exit %d\n%s", code, res)
	}
	if help := f.mustGraft("help"); !strings.Contains(help, "one -- <backup.tar.gz>  restore a backup") {
		t.Errorf("help:\n%s", help)
	}
	if tasks := f.mustGraft("tasks"); !strings.Contains(tasks, "all [-- args...]") {
		t.Errorf("tasks:\n%s", tasks)
	}
}

// fakeSystemd puts systemctl and journalctl on PATH that keep a unit's state
// in a file.
func fakeSystemd(t *testing.T, f *fixture, startState string) string {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.WriteFile(state, []byte("inactive"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	systemctl := `#!/bin/sh
echo "$*" >> ` + calls + `
case "$2" in
show) s=$(cat ` + state + `); pid=0; [ "$s" = active ] && pid=4242; echo "MainPID=$pid"; echo "ActiveState=$s" ;;
start) echo ` + startState + ` > ` + state + ` ;;
stop) echo inactive > ` + state + ` ;;
esac
`
	for name, body := range map[string]string{"systemctl": systemctl, "journalctl": "#!/bin/sh\necho \"journal $*\"\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.env = []string{"PATH=" + dir + string(os.PathListSeparator) + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	return calls
}

func TestSystemdServices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd is Linux")
	}
	f := tasksRepo(t, `services:
  web:
    desc: web through systemd
    systemd_unit: web.service
`, nil)
	calls := fakeSystemd(t, f, "active")
	if out := f.mustGraft("status"); !strings.Contains(out, "web: stopped, log journalctl --user -u web.service") {
		t.Errorf("status:\n%s", out)
	}
	if out := f.mustGraft("start"); !strings.Contains(out, "started web (web.service)") {
		t.Errorf("start:\n%s", out)
	}
	if out := f.mustGraft("start", "web"); !strings.Contains(out, "already running (web.service, pid 4242)") {
		t.Errorf("second start:\n%s", out)
	}
	if out := f.mustGraft("status"); !strings.Contains(out, "web: running (pid 4242)") {
		t.Errorf("status:\n%s", out)
	}
	if out := f.mustGraft("logs", "web", "--lines", "5"); !strings.Contains(out, "journal --user -u web.service -n 5 --no-pager") {
		t.Errorf("logs:\n%s", out)
	}
	if out := f.mustGraft("stop"); !strings.Contains(out, "stopped web") {
		t.Errorf("stop:\n%s", out)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--user start web.service") || !strings.Contains(string(data), "--user stop web.service") {
		t.Errorf("systemctl calls:\n%s", data)
	}

	g := tasksRepo(t, `services:
  web:
    systemd_unit: web.service
    start_timeout: 2s
`, nil)
	fakeSystemd(t, g, "failed")
	if out, code := g.graft("", "start"); code != 1 || !strings.Contains(out, "web.service failed (see graft logs web)") {
		t.Errorf("failing unit: exit %d\n%s", code, out)
	}
}

func TestServiceLogsFromFile(t *testing.T) {
	f, _ := serviceRepo(t, "fail", "")
	f.graft("", "start")
	out := f.mustGraft("logs", "web", "--lines", "1")
	if strings.TrimSpace(out) != "gatehelper: failing on purpose" {
		t.Errorf("logs:\n%s", out)
	}
	if res, code := f.graft("", "logs", "nosuch"); code != 1 || !strings.Contains(res, `unknown service "nosuch"`) {
		t.Errorf("unknown service: exit %d\n%s", code, res)
	}
}
