package app

import (
	"strings"
	"testing"
)

func TestReplaceFreezeJournalUsesConfiguredHeading(t *testing.T) {
	build := "<h2>Build 1.2.10</h2>"
	heading := "<h3>Release notes log</h3>"
	body := build + "<ul><li>items</li></ul>" + heading + "<ul><li>old</li></ul>"
	journal := heading + "<ul><li>new</li></ul>"
	body = replaceFreezeJournal(body, build, heading, journal)
	body = replaceFreezeJournal(body, build, heading, journal)
	if count := strings.Count(body, heading); count != 1 {
		t.Errorf("configured journal headings after two updates = %d:\n%s", count, body)
	}
	if !strings.Contains(body, "<li>new</li>") || strings.Contains(body, "<li>old</li>") {
		t.Errorf("configured journal was not replaced:\n%s", body)
	}
}
