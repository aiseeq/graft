package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Hotfix lets a branch from the commit production runs go out past the
// release window, the hold on the candidate and approved_release.
type Hotfix struct {
	// Branches are the globs of hotfix branch names, such as hotfix/*.
	Branches []string `yaml:"branches"`
	// Label is the label every item a hotfix commit names must carry.
	Label string `yaml:"label"`
	// Base is the target whose deployed commit a hotfix branch starts from.
	Base string `yaml:"base"`
	// Match is Branches compiled.
	Match []*regexp.Regexp `yaml:"-"`
}

// ReleaseVersion keeps one Jira version per window: the release writes its
// build into the version, releases it, renames it to the day it went out on,
// and adds the next window's version; a hotfix gets a version of its own.
type ReleaseVersion struct {
	// Enabled switches it on and off without dropping the rest; required.
	Enabled *bool `yaml:"enabled"`
	// Name is the window version template, such as "Product {dd}.{mm}.{yy}".
	Name string `yaml:"name"`
	// Hotfix is the template of the version a hotfix goes into, such as
	// "Product {dd}.{mm}.{yy} hotfix"; without it versions stay as they are.
	Hotfix string `yaml:"hotfix"`
	// Template and HotfixTemplate are Name and Hotfix compiled.
	Template       *VersionName `yaml:"-"`
	HotfixTemplate *VersionName `yaml:"-"`
}

// ReleasePage is the Confluence page a release publishes: what went out,
// one page per release, under Parent.
type ReleasePage struct {
	// Parent is the id of the page the release pages go under.
	Parent string `yaml:"parent"`
	// Title is the page title; {release} is the window's version, the
	// hotfix's, or the approved versions.
	Title string `yaml:"title"`
	// FixTypes are the issue types listed as fixes; the rest are changes.
	FixTypes []string `yaml:"fix_types"`
	// Labels are the page's words, by PageLabels key.
	Labels map[string]string `yaml:"labels"`
}

// PageLabels are the words of a release page and their defaults; build
// takes {version} and {short}.
var PageLabels = map[string]string{
	"release":        "Release",
	"date":           "Date",
	"target":         "Target",
	"build":          "Build {version}, commit {short}",
	"changes":        "Changes",
	"fixes":          "Fixes",
	"moved":          "Moved to the next release",
	"none":           "No items.",
	"freeze_journal": "Freeze journal",
	"issue":          "Issue",
	"summary":        "Summary",
	"type":           "Type",
}

// Label is the page's word for the key.
func (p *ReleasePage) Label(key string) string {
	if l, ok := p.Labels[key]; ok {
		return l
	}
	return PageLabels[key]
}

// On tells whether the versions are kept.
func (v *ReleaseVersion) On() bool { return v != nil && v.Enabled != nil && *v.Enabled }

// Window is when a target takes releases: on Days from At until Until in TZ.
type Window struct {
	Days  []string `yaml:"days"`
	At    string   `yaml:"at"`
	Until string   `yaml:"until"`
	TZ    string   `yaml:"tz"`
	// Cutoff, when set, makes the target this one requires hold the release
	// candidate from the cutoff until the window ends or this target runs it.
	Cutoff *Cutoff `yaml:"cutoff"`

	// Weekdays, Start, End and Location are the fields parsed; Start and End
	// are offsets from midnight.
	Weekdays   []time.Weekday `yaml:"-"`
	Start, End time.Duration  `yaml:"-"`
	Location   *time.Location `yaml:"-"`
}

// Cutoff is the moment the candidate is fixed: DaysBefore days before the
// window's day, at At.
type Cutoff struct {
	DaysBefore int    `yaml:"days_before"`
	At         string `yaml:"at"`
	// Time is At parsed, an offset from midnight.
	Time time.Duration `yaml:"-"`
}

var weekdays = map[string]time.Weekday{"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday}

var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// parseClock reads HH:MM as an offset from midnight.
func parseClock(where, s string) (time.Duration, error) {
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%s: %q is not HH:MM", where, s)
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", where, err)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

func (c *Config) validateWindow(where string, t *DeployTarget) error {
	w := t.Window
	if w == nil {
		return nil
	}
	where += ".window"
	if len(w.Days) == 0 {
		return fmt.Errorf("%s.days: required, such as [mon, thu]", where)
	}
	w.Weekdays = nil
	for _, d := range w.Days {
		wd, ok := weekdays[d]
		if !ok {
			return fmt.Errorf("%s.days: %q is not one of mon, tue, wed, thu, fri, sat, sun", where, d)
		}
		if slices.Contains(w.Weekdays, wd) {
			return fmt.Errorf("%s.days: %s twice", where, d)
		}
		w.Weekdays = append(w.Weekdays, wd)
	}
	var err error
	if w.Start, err = parseClock(where+".at", w.At); err != nil {
		return err
	}
	if w.End, err = parseClock(where+".until", w.Until); err != nil {
		return err
	}
	if w.End <= w.Start {
		return fmt.Errorf("%s.until: %s is not after at %s", where, w.Until, w.At)
	}
	if strings.TrimSpace(w.TZ) == "" {
		return fmt.Errorf("%s.tz: required, such as Europe/Berlin", where)
	}
	if w.Location, err = time.LoadLocation(w.TZ); err != nil {
		return fmt.Errorf("%s.tz: %w", where, err)
	}
	if t.DeployedSHA == nil {
		return fmt.Errorf("%s: needs deployed_sha to know what the target runs", where)
	}
	return c.validateCutoff(where, t)
}

func (c *Config) validateCutoff(where string, t *DeployTarget) error {
	cut := t.Window.Cutoff
	if cut == nil {
		return nil
	}
	where += ".cutoff"
	var err error
	if cut.Time, err = parseClock(where+".at", cut.At); err != nil {
		return err
	}
	if cut.DaysBefore < 0 {
		return fmt.Errorf("%s.days_before: %d is negative", where, cut.DaysBefore)
	}
	if time.Duration(cut.DaysBefore)*24*time.Hour+t.Window.Start-cut.Time <= 0 {
		return fmt.Errorf("%s: %s %d days before is not before the window opens at %s", where, cut.At, cut.DaysBefore, t.Window.At)
	}
	if t.Requires == "" {
		return fmt.Errorf("%s: needs requires, the target that holds the candidate", where)
	}
	if held := c.Deploy.Targets[t.Requires]; held != nil && held.DeployedSHA == nil {
		return fmt.Errorf("%s: target %q holds the candidate and needs deployed_sha", where, t.Requires)
	}
	return nil
}

func (c *Config) validateReleaseVersion(where string, t *DeployTarget) error {
	v := t.ReleaseVersion
	if v == nil {
		return nil
	}
	where += ".release_version"
	var err error
	switch {
	case v.Enabled == nil:
		return fmt.Errorf("%s.enabled: required, true or false (false keeps the section and leaves versions alone)", where)
	case t.Window == nil:
		return fmt.Errorf("%s: needs window, whose days name the versions", where)
	case c.ReleaseNotes == nil || c.ReleaseNotes.Jira == nil:
		return fmt.Errorf("%s: needs a top-level release_notes.jira section for the project keys", where)
	}
	if v.Template, err = ParseVersionName(v.Name); err != nil {
		return fmt.Errorf("%s.name: %w", where, err)
	}
	if v.Hotfix != "" {
		if v.HotfixTemplate, err = ParseVersionName(v.Hotfix); err != nil {
			return fmt.Errorf("%s.hotfix: %w", where, err)
		}
	}
	return nil
}

var pageIDRe = regexp.MustCompile(`^[0-9]+$`)

func (c *Config) validateReleasePage(where string, t *DeployTarget) error {
	p := t.ReleasePage
	if p == nil {
		return nil
	}
	where += ".release_page"
	switch {
	case !pageIDRe.MatchString(p.Parent):
		return fmt.Errorf("%s.parent: required, the numeric id of the page release pages go under", where)
	case t.ReleaseVersion == nil && (t.ApprovedRelease == nil || t.ApprovedRelease.Prefix == ""):
		return fmt.Errorf("%s: needs release_version or approved_release.prefix, whose versions name the release", where)
	}
	if p.Title == "" {
		p.Title = "{release}: release notes"
	}
	if !strings.Contains(p.Title, "{release}") {
		return fmt.Errorf("%s.title: %q needs {release}: a title is unique in a space", where, p.Title)
	}
	if len(p.FixTypes) == 0 {
		p.FixTypes = []string{"Bug"}
	}
	for k := range p.Labels {
		if _, ok := PageLabels[k]; !ok {
			return fmt.Errorf("%s.labels: unknown %q (%s)", where, k, strings.Join(keysOf(PageLabels), ", "))
		}
	}
	return nil
}

func (c *Config) validateHotfix() error {
	h := c.Deploy.Hotfix
	if h == nil {
		return nil
	}
	if len(h.Branches) == 0 {
		return errors.New("deploy.hotfix.branches: required, such as [hotfix/*]")
	}
	match, err := compileBranchGlobs("deploy.hotfix.branches", h.Branches)
	if err != nil {
		return err
	}
	h.Match = match
	switch base := c.Deploy.Targets[h.Base]; {
	case strings.TrimSpace(h.Label) == "":
		return errors.New("deploy.hotfix.label: required, the label of hotfix items, such as hotfix")
	case h.Base == "":
		return errors.New("deploy.hotfix.base: required, the target whose deployed commit a hotfix branch starts from")
	case base == nil:
		return fmt.Errorf("deploy.hotfix.base: unknown target %q", h.Base)
	case base.DeployedSHA == nil:
		return fmt.Errorf("deploy.hotfix.base: target %q has no deployed_sha to start from", h.Base)
	case c.Deploy.Branch != "":
		return fmt.Errorf("deploy.hotfix: deploy.branch %q is the only branch deployed, a hotfix branch never would be", c.Deploy.Branch)
	case c.ReleaseNotes == nil || c.ReleaseNotes.Jira == nil:
		return errors.New("deploy.hotfix: needs a top-level release_notes.jira section for the project keys")
	}
	return nil
}

// HoldingWindows lists the targets whose window cutoff makes the named target
// hold their release candidate.
func (d *Deploy) HoldingWindows(name string) []string {
	var out []string
	for _, n := range keysOf(d.Targets) {
		t := d.Targets[n]
		if t.Requires == name && t.Window != nil && t.Window.Cutoff != nil {
			out = append(out, n)
		}
	}
	return out
}

// IsHotfix tells whether the branch is a hotfix branch.
func (d *Deploy) IsHotfix(branch string) bool {
	return d.Hotfix != nil && BranchMatches(d.Hotfix.Match, branch)
}
