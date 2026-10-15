package deploy

import (
	"strings"
	"testing"
	"time"

	"github.com/aiseeq/graft/internal/config"
)

func testWindow(t *testing.T, yaml string) *config.Window {
	t.Helper()
	cfg, err := config.Parse([]byte(`schema: 1
version: {mode: file}
envs: {local: {}}
deploy:
  remote: origin
  targets:
    test: {env: local, run: [sh, d.sh], version: 'cat V', deployed_sha: {path: /x/test}}
    prod: {env: local, run: [sh, d.sh], requires: test, deployed_sha: {path: /x/prod}, window: ` + yaml + `}
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Deploy.Targets["prod"].Window
}

// at reads a time in UTC+3 (the window's zone in these tests).
func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02 15:04 -0700", s+" +0300")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWindowOpen(t *testing.T) {
	w := testWindow(t, `{days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3}`)
	// 2026-10-19 is a Monday.
	for now, want := range map[string]bool{
		"2026-10-19 15:00": true,
		"2026-10-19 18:59": true,
		"2026-10-19 19:00": false,
		"2026-10-19 14:59": false,
		"2026-10-20 16:00": false,
		"2026-10-22 16:00": true,
	} {
		if got := Open(w, at(t, now)); got != want {
			t.Errorf("Open at %s = %v, want %v", now, got, want)
		}
	}
	next := Next(w, at(t, "2026-10-19 19:00"))
	if got := next.Start.Format("Mon 02.01 15:04"); got != "Thu 22.10 15:00" {
		t.Errorf("next window after Monday's = %s", got)
	}
	if got := Next(w, at(t, "2026-10-19 16:00")).Start.Format("02.01"); got != "19.10" {
		t.Errorf("the open window is the next one: %s", got)
	}
	if got := Describe(w); got != "mon, thu 15:00-19:00 Etc/GMT-3" {
		t.Errorf("Describe = %q", got)
	}
}

func TestHold(t *testing.T) {
	w := testWindow(t, `{days: [mon, thu], at: "15:00", until: "19:00", tz: Etc/GMT-3, cutoff: {days_before: 1, at: "18:00"}}`)
	for now, want := range map[string]string{
		"2026-10-18 17:59": "",
		"2026-10-18 18:00": "2026-10-19",
		"2026-10-19 18:59": "2026-10-19",
		"2026-10-19 19:00": "",
		"2026-10-21 18:30": "2026-10-22",
		// Friday's cutoff would be Thursday 18:00, but Friday has no window.
		"2026-10-22 18:30": "2026-10-22",
		"2026-10-22 19:30": "",
	} {
		occ, held := Hold(w, at(t, now))
		got := ""
		if held {
			got = occ.Date
		}
		if got != want {
			t.Errorf("Hold at %s = %q, want %q", now, got, want)
		}
	}
	occ, _ := Hold(w, at(t, "2026-10-18 20:00"))
	if c := occ.Cutoff.Format("Mon 02.01 15:04"); c != "Sun 18.10 18:00" || !strings.HasPrefix(occ.Name(), "19.10.26") {
		t.Errorf("cutoff %s, name %s", c, occ.Name())
	}
}
