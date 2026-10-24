package config

import (
	"slices"
	"strings"
	"testing"
)

func TestParseRemoteHeavy(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "remote_heavy: {host: pc, up: [pc, up], run: [pc, run], threads: 8}\ntasks:\n  build:\n    heavy: true\n    remote: false\n    run: [go build]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.RemoteHeavy
	if r.Host != "pc" || !slices.Equal(r.Up, []string{"pc", "up"}) || !slices.Equal(r.Run, []string{"pc", "run"}) || r.Threads != 8 {
		t.Errorf("remote_heavy: %+v", r)
	}
	if rm := cfg.Tasks["build"].Remote; rm == nil || *rm {
		t.Errorf("remote: %v", rm)
	}
}

func TestParseRemoteHeavyErrors(t *testing.T) {
	const ok = "{host: pc, up: [pc, up], run: [pc, run]}"
	cases := map[string]struct{ data, want string }{
		"no host":           {"remote_heavy: {up: [a], run: [b]}\n", "remote_heavy.host"},
		"no up":             {"remote_heavy: {host: pc, run: [b]}\n", "remote_heavy.up"},
		"empty up":          {"remote_heavy: {host: pc, up: [''], run: [b]}\n", "remote_heavy.up"},
		"no run":            {"remote_heavy: {host: pc, up: [a]}\n", "remote_heavy.run"},
		"negative threads":  {"remote_heavy: {host: pc, up: [a], run: [b], threads: -1}\n", "remote_heavy.threads"},
		"remote, not heavy": {"remote_heavy: " + ok + "\ntasks:\n  lint:\n    remote: true\n    run: [go vet]\n", "tasks.lint.remote"},
		"unknown key":       {"remote_heavy: {host: pc, up: [a], run: [b], dir: x}\n", "dir"},
	}
	for name, c := range cases {
		_, err := Parse([]byte(minimal + c.data))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %s", name, err, c.want)
		}
	}
}
