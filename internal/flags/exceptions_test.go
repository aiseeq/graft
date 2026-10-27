package flags

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var knownEnvs = []string{"local", "prod", "test"}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules([]byte(`# comment

*|http_failure|timeout calling provider|provider is flaky, retried
prod,test | log_error | disk almost full | monitored elsewhere
local|job_stalled|*|local jobs stall on laptop sleep
*|request_rejected|a|b|reason with | pipe
`), knownEnvs)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("%d rules", len(rules))
	}
	if rules[1].Class != "log_error" || rules[1].Substring != "disk almost full" || len(rules[1].Envs) != 2 {
		t.Errorf("trimmed rule: %+v", rules[1])
	}
	if rules[3].Substring != "a" || rules[3].Reason != "b|reason with | pipe" {
		t.Errorf("reason with pipes: %+v", rules[3])
	}
	ev := Event{Class: "http_failure", Subject: "POST /pay: timeout calling provider X"}
	if !rules[0].Matches("prod", ev) || !rules[0].Matches("local", ev) {
		t.Error("* env rule must match everywhere")
	}
	disk := Event{Class: "log_error", Subject: "disk almost full on /var"}
	if !rules[1].Matches("test", disk) || rules[1].Matches("local", disk) {
		t.Error("env list")
	}
	if !rules[2].Matches("local", Event{Class: "job_stalled", Subject: "anything"}) {
		t.Error("whole class")
	}
	if rules[0].Matches("prod", Event{Class: "http_failure", Subject: "TIMEOUT CALLING PROVIDER"}) {
		t.Error("match must be case-sensitive")
	}
	if rules[0].Matches("prod", Event{Class: "other", Subject: ev.Subject}) {
		t.Error("class must match")
	}
	if !(Rule{Class: "c", Substring: "50%_done"}).Matches("x", Event{Class: "c", Subject: "at 50%_done"}) ||
		(Rule{Class: "c", Substring: "50%_done"}).Matches("x", Event{Class: "c", Subject: "at 50xydone"}) {
		t.Error("% and _ are literal, not LIKE wildcards")
	}
}

func TestParseRulesField(t *testing.T) {
	rules, err := ParseRules([]byte(`test|log_error|body:userID=f00d|one account
*|log_error|key:balance:42|one key
*|log_error|subject:body: of the request|a subject starting with body:
`), knownEnvs)
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].In != InBody || rules[0].Substring != "userID=f00d" || rules[1].In != InKey || rules[1].Substring != "balance:42" {
		t.Errorf("field rules: %+v", rules[:2])
	}
	if rules[2].In != InSubject || rules[2].Substring != "body: of the request" {
		t.Errorf("subject: rule: %+v", rules[2])
	}
	ev := Event{Class: "log_error", Subject: "negative total", Body: "total=-1 userID=f00d", Key: "balance:42"}
	if !rules[0].Matches("test", ev) || rules[0].Matches("test", Event{Class: "log_error", Subject: "userID=f00d", Body: "userID=beef"}) {
		t.Error("a body rule looks in the body only")
	}
	if !rules[1].Matches("prod", ev) || rules[1].Matches("prod", Event{Class: "log_error", Subject: "balance:42"}) {
		t.Error("a key rule looks in the key only")
	}
	if !rules[2].Matches("prod", Event{Class: "log_error", Subject: "body: of the request too big"}) {
		t.Error("subject: rule")
	}
	for i, want := range []string{"test|log_error|body:userID=f00d|one account", "*|log_error|key:balance:42|one key",
		"*|log_error|subject:body: of the request|a subject starting with body:"} {
		if got := rules[i].String(); got != want {
			t.Errorf("rule %d renders %q, want %q", i, got, want)
		}
	}
}

func TestParseRulesErrors(t *testing.T) {
	for name, line := range map[string]string{
		"three fields":    "log_error|disk|reason",
		"empty env":       "|log_error|disk|reason",
		"empty class":     "*||disk|reason",
		"empty substring": "*|log_error||reason",
		"empty reason":    "*|log_error|disk|",
		"unknown env":     "staging|log_error|disk|reason",
		"empty body":      "*|log_error|body:|reason",
		"whole body":      "*|log_error|body:*|reason",
	} {
		if _, err := ParseRules([]byte(line+"\n"), knownEnvs); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	_, err := ParseRules([]byte("*|a|b|c\nbad\nalso|bad\n"), knownEnvs)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("every bad line must be reported: %v", err)
	}
}

func TestAppendRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions.conf")
	if err := os.WriteFile(path, []byte("# rules\n*|a|b|c"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Rule{Envs: []string{"prod"}, Class: "log_error", Substring: "disk full", Reason: "noise"}
	added, err := AppendRule(path, r, knownEnvs)
	if err != nil || !added {
		t.Fatalf("append: %v %v", added, err)
	}
	added, err = AppendRule(path, r, knownEnvs)
	if err != nil || added {
		t.Errorf("duplicate appended: %v %v", added, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "# rules\n*|a|b|c\nprod|log_error|disk full|noise\n" {
		t.Errorf("file:\n%s", data)
	}
	if _, err := AppendRule(path, Rule{Class: "x", Substring: "a|b", Reason: "r"}, knownEnvs); err == nil {
		t.Error("| in substring accepted")
	}
}
