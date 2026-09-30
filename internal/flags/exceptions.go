// Package flags implements graft flags: a journal of events that need a human
// look, read from a project adapter, filtered by an exceptions file, and
// closed with a reason.
package flags

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Rule mutes events of a class whose subject contains Substring, in the
// listed environments. Substring "*" mutes the whole class.
type Rule struct {
	Envs      []string // nil means every environment ("*")
	Class     string
	Substring string
	Reason    string
	Line      int
}

// AllEnvs is the env field value that applies a rule everywhere.
const AllEnvs = "*"

// WholeClass is the substring value that mutes every event of a class.
const WholeClass = "*"

// Matches reports whether the rule mutes the event in env. The substring is
// compared literally and case-sensitively.
func (r Rule) Matches(env string, e Event) bool {
	if r.Envs != nil && !slices.Contains(r.Envs, env) {
		return false
	}
	if r.Class != e.Class {
		return false
	}
	return r.Substring == WholeClass || strings.Contains(e.Subject, r.Substring)
}

// String renders the rule as a line of the exceptions file.
func (r Rule) String() string {
	env := AllEnvs
	if r.Envs != nil {
		env = strings.Join(r.Envs, ",")
	}
	return strings.Join([]string{env, r.Class, r.Substring, r.Reason}, "|")
}

// ParseRules reads an exceptions file: one rule per line,
// env|class|substring|reason, blank lines and # comments ignored. env is * or a
// comma-separated list of known environments. Every field is required; the
// reason may itself contain |. Any malformed line is an error: a rule that
// silently fails to parse leaves the event it was meant to mute raising the
// flag, or worse, a rule meant for one environment muting all of them.
func ParseRules(data []byte, knownEnvs []string) ([]Rule, error) {
	var (
		rules []Rule
		errs  []error
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parseRule(line, knownEnvs)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", n, err))
			continue
		}
		r.Line = n
		rules = append(rules, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the rules: %w", err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return rules, nil
}

func parseRule(line string, knownEnvs []string) (Rule, error) {
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return Rule{}, fmt.Errorf("want env|class|substring|reason, got %q", line)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	names := []string{"env", "class", "substring", "reason"}
	for i, p := range parts {
		if p == "" {
			return Rule{}, fmt.Errorf("empty %s in %q (a whole class is muted with substring *)", names[i], line)
		}
	}
	r := Rule{Class: parts[1], Substring: parts[2], Reason: parts[3]}
	if parts[0] != AllEnvs {
		for env := range strings.SplitSeq(parts[0], ",") {
			env = strings.TrimSpace(env)
			if !slices.Contains(knownEnvs, env) {
				return Rule{}, fmt.Errorf("unknown env %q in %q (known: %s)", env, line, strings.Join(knownEnvs, ", "))
			}
			r.Envs = append(r.Envs, env)
		}
	}
	return r, nil
}

// LoadRules reads the exceptions file at path. A missing file is an error: the
// config names it, and graft flags mute needs it to exist.
func LoadRules(path string, knownEnvs []string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("exceptions file: %w", err)
	}
	rules, err := ParseRules(data, knownEnvs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}

// AppendRule adds r to the exceptions file unless an identical env, class and
// substring are already there. It reports whether the file changed.
func AppendRule(path string, r Rule, knownEnvs []string) (bool, error) {
	if strings.Contains(r.Substring, "|") || strings.Contains(r.Class, "|") {
		return false, errors.New("the rule's class and substring cannot contain |: pass a narrower --match")
	}
	if strings.ContainsAny(r.Substring+r.Reason, "\r\n") {
		return false, errors.New("the rule's substring and reason must be a single line")
	}
	existing, err := LoadRules(path, knownEnvs)
	if err != nil {
		return false, err
	}
	for _, e := range existing {
		if e.Class == r.Class && e.Substring == r.Substring && slices.Equal(e.Envs, r.Envs) {
			return false, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("appending a rule: %w", err)
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	data = append(data, []byte(r.String()+"\n")...)
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("appending a rule: %w", err)
	}
	return true, os.WriteFile(path, data, info.Mode().Perm())
}
