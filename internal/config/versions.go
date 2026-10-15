package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// VersionName is a version name template with the window's day in it, such
// as "Product {dd}.{mm}.{yy}": {dd}, {mm} and {yy} each once.
type VersionName struct {
	Template string
	re       *regexp.Regexp
	// order is where day, month and year come in the name.
	order []string
}

var namePlaceholderRe = regexp.MustCompile(`\{(dd|mm|yy)\}`)

// ParseVersionName compiles a template.
func ParseVersionName(template string) (*VersionName, error) {
	if strings.TrimSpace(template) == "" {
		return nil, errors.New("required, such as \"Product {dd}.{mm}.{yy}\"")
	}
	n := &VersionName{Template: template}
	var pattern strings.Builder
	pattern.WriteString("^")
	last := 0
	for _, m := range namePlaceholderRe.FindAllStringSubmatchIndex(template, -1) {
		part := template[m[2]:m[3]]
		if strings.Contains(strings.Join(n.order, " "), part) {
			return nil, fmt.Errorf("%q has {%s} twice", template, part)
		}
		n.order = append(n.order, part)
		pattern.WriteString(regexp.QuoteMeta(template[last:m[0]]) + `([0-9]{2})`)
		last = m[1]
	}
	pattern.WriteString(regexp.QuoteMeta(template[last:]) + "$")
	if len(n.order) != 3 {
		return nil, fmt.Errorf("%q needs {dd}, {mm} and {yy}", template)
	}
	n.re = regexp.MustCompile(pattern.String())
	return n, nil
}

// Format is the name for the day.
func (n *VersionName) Format(day time.Time) string {
	return strings.NewReplacer("{dd}", day.Format("02"), "{mm}", day.Format("01"), "{yy}", day.Format("06")).Replace(n.Template)
}

// Date reads the day from a name that follows the template exactly.
func (n *VersionName) Date(name string) (time.Time, bool) {
	m := n.re.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	return n.day(m[1:])
}

// day reads the day from its parts in the template's order; an impossible
// day, 31.02, is none.
func (n *VersionName) day(groups []string) (time.Time, bool) {
	parts := map[string]string{}
	for i, p := range n.order {
		parts[p] = groups[i]
	}
	day, err := time.Parse("02.01.06", parts["dd"]+"."+parts["mm"]+"."+parts["yy"])
	return day, err == nil
}

var looseDateRe = regexp.MustCompile(`[0-9]{2}[^0-9a-zA-Z][0-9]{2}[^0-9a-zA-Z][0-9]{2}`)

// LooksLike tells whether a name that does not follow the template is a slip
// of it, "Product.19.10.26" for "Product {dd}.{mm}.{yy}": the same letters and
// digits, other separators.
func (n *VersionName) LooksLike(name string) bool {
	if _, ok := n.Date(name); ok {
		return false
	}
	for _, d := range looseDateRe.FindAllString(name, -1) {
		day, ok := n.day([]string{d[0:2], d[3:5], d[6:8]})
		if ok && alnum(n.Format(day)) == alnum(name) {
			return true
		}
	}
	return false
}

func alnum(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
