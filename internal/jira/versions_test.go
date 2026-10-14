package jira

import (
	"strings"
	"testing"
)

func TestNextVersion(t *testing.T) {
	v := func(id, name, date string) Version { return Version{ID: id, Name: name, ReleaseDate: date} }
	released := v("1", "Product v1.2.1", "2026-01-01")
	released.Released = true
	archived := v("2", "Product v1.2.2", "2026-02-01")
	archived.Archived = true
	cases := []struct {
		name     string
		versions []Version
		want     string
		errHas   []string
	}{
		{
			name:     "nearest date wins, released, archived and other prefixes are out",
			versions: []Version{released, archived, v("3", "Product v1.4.0", "2026-12-01"), v("4", "Other v1.2.3", "2026-10-01"), v("5", "Product v1.3.0", "2026-11-01")},
			want:     "5",
		},
		{
			name:     "a dated version comes before an undated one",
			versions: []Version{v("6", "Product v2.0.0", ""), v("7", "Product v1.9.0", "2027-06-01")},
			want:     "7",
		},
		{
			name:     "a single undated version",
			versions: []Version{released, v("8", "Product v2.0.0", "")},
			want:     "8",
		},
		{
			name:     "none matches",
			versions: []Version{released, archived, v("4", "Other v1.2.3", "2026-10-01")},
			errHas:   []string{`no unreleased version of PROJ starts with "Product v"`, "Other v1.2.3"},
		},
		{
			name:     "no unreleased versions at all",
			versions: []Version{released},
			errHas:   []string{"PROJ has no unreleased versions"},
		},
		{
			name:     "two share the nearest date",
			versions: []Version{v("3", "Product v1.4.0", "2026-12-01"), v("9", "Product v1.3.0", "2026-11-01"), v("10", "Product v1.3.1", "2026-11-01")},
			errHas:   []string{"share the nearest release date 2026-11-01", "Product v1.3.0, Product v1.3.1"},
		},
		{
			name:     "several undated and no dated",
			versions: []Version{v("6", "Product v2.0.0", ""), v("11", "Product v3.0.0", "")},
			errHas:   []string{"none has a release date", "Product v2.0.0, Product v3.0.0"},
		},
		{
			name: "a frozen version is passed over",
			versions: func() []Version {
				frozen := v("5", "Product v1.3.0", "2026-11-01")
				frozen.Frozen = true
				return []Version{frozen, v("3", "Product v1.4.0", "2026-12-01")}
			}(),
			want: "3",
		},
		{
			name: "only frozen versions left",
			versions: func() []Version {
				frozen := v("5", "Product v1.3.0", "2026-11-01")
				frozen.Frozen = true
				return []Version{released, frozen}
			}(),
			errHas: []string{`every unreleased version of PROJ starting with "Product v" is being approved`, "Product v1.3.0", "add the next version"},
		},
		{
			name:     "a malformed date",
			versions: []Version{v("12", "Product v1.3.0", "next week")},
			errHas:   []string{`"Product v1.3.0" has release date "next week"`},
		},
	}
	for _, c := range cases {
		got, err := NextVersion("PROJ", c.versions, "Product v")
		if c.errHas == nil {
			if err != nil || got.ID != c.want {
				t.Errorf("%s: %+v %v, want id %s", c.name, got, err, c.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: got %+v, want an error", c.name, got)
			continue
		}
		for _, s := range c.errHas {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: %q lacks %q", c.name, err, s)
			}
		}
	}
}

func TestApprovals(t *testing.T) {
	a := func(statuses ...string) []Approver {
		var out []Approver
		for i, s := range statuses {
			out = append(out, Approver{AccountID: string(rune('a' + i)), Status: s})
		}
		return out
	}
	cases := []struct {
		name         string
		approvers    []Approver
		begun, fully bool
	}{
		{"no approvers", nil, false, false},
		{"pending", a("PENDING"), false, false},
		{"one of two", a("APPROVED", "PENDING"), true, false},
		{"approved and declined", a("APPROVED", "DECLINED"), true, false},
		{"all", a("APPROVED", "APPROVED"), true, true},
	}
	for _, c := range cases {
		if got := ApprovalBegun(c.approvers); got != c.begun {
			t.Errorf("%s: ApprovalBegun = %v", c.name, got)
		}
		if got := FullyApproved(c.approvers); got != c.fully {
			t.Errorf("%s: FullyApproved = %v", c.name, got)
		}
	}
}
