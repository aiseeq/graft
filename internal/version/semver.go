// Package version reads, bumps and propagates the project version.
package version

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// Semver is a MAJOR.MINOR.PATCH version without pre-release or build parts,
// or a MAJOR.MINOR.PATCH.BUILD version of a build branch (version.build_branches).
type Semver struct {
	Major, Minor, Patch int
	// Build is the fourth part, 0 for none.
	Build int
}

// Level selects which part of the version a bump increments.
type Level int

// Bump levels.
const (
	Patch Level = iota
	Minor
	Major
)

func (l Level) String() string {
	switch l {
	case Minor:
		return "minor"
	case Major:
		return "major"
	case Patch:
		return "patch"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

var semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// IsPlain reports whether s is a MAJOR.MINOR.PATCH version that Parse accepts
// the shape of.
func IsPlain(s string) bool { return semverRe.MatchString(s) }

var buildRe = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)(?:[-+].*)?$`)

// ParseBuild returns the release a build's version string starts from:
// v0.4.0, v0.4.0-3-gabc1234-dirty (git describe) and Go pseudo-versions all
// carry one. Builds without one (a bare commit hash, "(devel)") are errors.
func ParseBuild(s string) (Semver, error) {
	m := buildRe.FindStringSubmatch(s)
	if m == nil {
		return Semver{}, fmt.Errorf("%q does not start with a MAJOR.MINOR.PATCH version", s)
	}
	return Parse(m[1])
}

var fileRe = regexp.MustCompile(`^((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))(?:\.([1-9][0-9]*))?$`)

// ParseFile accepts what a version file holds: MAJOR.MINOR.PATCH, or
// MAJOR.MINOR.PATCH.BUILD with BUILD from 1 on a build branch.
func ParseFile(s string) (Semver, error) {
	m := fileRe.FindStringSubmatch(s)
	if m == nil {
		return Semver{}, fmt.Errorf("%q is not a MAJOR.MINOR.PATCH or MAJOR.MINOR.PATCH.BUILD version", s)
	}
	v, err := Parse(m[1])
	if err != nil || m[2] == "" {
		return v, err
	}
	if v.Build, err = strconv.Atoi(m[2]); err != nil || v.Build == math.MaxInt {
		return Semver{}, fmt.Errorf("%q: build part %q out of range", s, m[2])
	}
	return v, nil
}

// Parse accepts exactly MAJOR.MINOR.PATCH: no prefix, no pre-release suffix,
// no leading zeros.
func Parse(s string) (Semver, error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return Semver{}, fmt.Errorf("%q is not a MAJOR.MINOR.PATCH version", s)
	}
	var parts [3]int
	for i := range parts {
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n == math.MaxInt {
			return Semver{}, fmt.Errorf("%q: version part %q out of range", s, m[i+1])
		}
		parts[i] = n
	}
	return Semver{Major: parts[0], Minor: parts[1], Patch: parts[2]}, nil
}

// BumpBuild returns the next build of the version: 1.2.3 -> 1.2.3.1 ->
// 1.2.3.2.
func (v Semver) BumpBuild() Semver {
	v.Build++
	return v
}

// Bump returns the next version at the given level; a build part is dropped.
func (v Semver) Bump(level Level) Semver {
	switch level {
	case Major:
		return Semver{Major: v.Major + 1}
	case Minor:
		return Semver{Major: v.Major, Minor: v.Minor + 1}
	case Patch:
		return Semver{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}
	default:
		panic(fmt.Sprintf("version: unknown bump level %d", int(level)))
	}
}

// Less orders versions by precedence.
func (v Semver) Less(o Semver) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	if v.Patch != o.Patch {
		return v.Patch < o.Patch
	}
	return v.Build < o.Build
}

func (v Semver) String() string {
	if v.Build > 0 {
		return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Patch, v.Build)
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}
