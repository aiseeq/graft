// Package leaks keeps private names out of public repositories: identifiers
// declared in the user's private Go projects and terms from a private list
// (work item keys, domains, addresses, counterparties). Both lists live
// outside the public repository, in the user's graft config, so the check
// itself leaks nothing.
package leaks

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Term is one entry of the private terms file.
type Term struct {
	Kind string
	Re   *regexp.Regexp
}

// Rules is what a scan looks for.
type Rules struct {
	// names maps a private identifier to the project that declares it.
	names map[string]string
	terms []Term
	// allow maps an allowed name or term match to its reason.
	allow map[string]string
	// modules are the module paths the private sources declare.
	modules []string
}

// Options configure the rules.
type Options struct {
	// Sources are the private projects whose Go declarations are private names.
	Sources []string
	// TermsFile lists private terms, one "kind: regexp" per line.
	TermsFile string
	// MinNameLength drops shorter identifiers: short names are common words.
	MinNameLength int
	// Allow exempts names or term matches, each with a reason.
	Allow map[string]string
}

// Load collects the private names and reads the terms.
func Load(o Options) (*Rules, error) {
	if o.MinNameLength < 1 {
		return nil, fmt.Errorf("the minimum name length must be positive, got %d", o.MinNameLength)
	}
	r := &Rules{names: map[string]string{}, allow: o.Allow}
	for _, src := range o.Sources {
		if err := r.collect(src, o.MinNameLength); err != nil {
			return nil, err
		}
	}
	if o.TermsFile != "" {
		terms, err := ReadTerms(o.TermsFile)
		if err != nil {
			return nil, err
		}
		r.terms = terms
	}
	return r, nil
}

// ReadTerms parses the terms file: "kind: regexp" per line, # comments.
func ReadTerms(path string) ([]Term, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the private terms: %w", err)
	}
	defer f.Close()
	var terms []Term
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, pattern, ok := strings.Cut(line, ": ")
		if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("%s:%d: want \"kind: regexp\"", path, n)
		}
		re, err := regexp.Compile(strings.TrimSpace(pattern))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		terms = append(terms, Term{Kind: strings.TrimSpace(kind), Re: re})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return terms, nil
}

// declRe finds the names a Go line declares: a function or method, a type,
// or a type inside a type ( ... ) group.
var (
	funcRe      = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)
	typeRe      = regexp.MustCompile(`^type\s+([A-Za-z_][A-Za-z0-9_]*)`)
	groupTypeRe = regexp.MustCompile(`^\t([A-Za-z_][A-Za-z0-9_]*)\s`)
)

// skipDirs are never private code of the project itself.
var skipDirs = []string{"vendor", "node_modules"}

func (r *Rules) collect(src string, minLen int) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("private source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("private source %s is not a directory", src)
	}
	project := filepath.Base(src)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != src && (strings.HasPrefix(d.Name(), ".") || slices.Contains(skipDirs, d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && d.Name() == "go.mod" {
			return r.readModule(path)
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range declarations(string(data)) {
			// A test name such as TestRepo_UpdateStatus_Keeps carries the
			// names of what it tests, which may be gone from the code; the
			// test's own name is a sentence anybody may write.
			for part := range strings.SplitSeq(name, "_") {
				if distinctive(part, minLen) && !isTestName(part) {
					if _, seen := r.names[part]; !seen {
						r.names[part] = project
					}
				}
			}
		}
		return nil
	})
}

// declarations returns the names Go source declares at the top level:
// functions, methods and types, grouped types included.
func declarations(src string) []string {
	var names []string
	inGroup := false
	for line := range strings.SplitSeq(src, "\n") {
		switch {
		case inGroup && strings.HasPrefix(line, ")"):
			inGroup = false
		case inGroup:
			if m := groupTypeRe.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			}
		case strings.HasPrefix(line, "type ("):
			inGroup = true
		default:
			if m := funcRe.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			} else if m := typeRe.FindStringSubmatch(line); m != nil {
				names = append(names, m[1])
			}
		}
	}
	return names
}

// isTestName matches the go test entry points: TestX, BenchmarkX, FuzzX,
// ExampleX.
func isTestName(s string) bool {
	for _, p := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(s, p); ok && rest != "" && unicode.IsUpper([]rune(rest)[0]) {
			return true
		}
	}
	return false
}

func (r *Rules) readModule(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			r.modules = append(r.modules, strings.Trim(strings.TrimSpace(rest), `"`))
			return nil
		}
	}
	return fmt.Errorf("%s: no module line", path)
}

// Modules lists the module paths of the private sources.
func (r *Rules) Modules() []string { return r.modules }

// DropCommon forgets the private names public Go code also uses: those are
// common words of the language's ecosystem, not the private project's own.
func (r *Rules) DropCommon(c *Corpus) {
	for name := range r.names {
		if c.Has(name) {
			delete(r.names, name)
		}
	}
}

// distinctive keeps camel-case identifiers of at least minLen: two or more
// words joined, the shape of a name somebody made up for their code.
func distinctive(s string, minLen int) bool {
	if len(s) < minLen {
		return false
	}
	runes := []rune(s)
	if !unicode.IsLetter(runes[0]) {
		return false
	}
	for i := 1; i < len(runes); i++ {
		if unicode.IsLower(runes[i-1]) && unicode.IsUpper(runes[i]) {
			return true
		}
	}
	return false
}

// Names reports how many private names the rules hold.
func (r *Rules) Names() int { return len(r.names) }

// Hit is one private name or term on a line.
type Hit struct {
	// Name is the private identifier; empty for a term.
	Name   string
	Text   string
	Reason string
}

// allowMarker on a line exempts it; the reason follows the marker.
const allowMarker = "graft:leak-ok"

// errNoReason is reported for a marker without a reason.
var errNoReason = errors.New(allowMarker + " needs a reason after it")

var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// ScanLine returns the private names and terms on one line. A line carrying
// graft:leak-ok with a reason has none.
func (r *Rules) ScanLine(line string) ([]Hit, error) {
	if _, after, ok := strings.Cut(line, allowMarker); ok {
		if strings.TrimSpace(strings.Trim(after, ":")) == "" {
			return nil, errNoReason
		}
		return nil, nil
	}
	var hits []Hit
	seen := map[string]bool{}
	for _, word := range wordRe.FindAllString(line, -1) {
		for part := range strings.SplitSeq(word, "_") {
			project, ok := r.names[part]
			if !ok || seen[part] {
				continue
			}
			seen[part] = true
			if _, allowed := r.allow[part]; allowed {
				continue
			}
			hits = append(hits, Hit{Name: part, Text: part, Reason: fmt.Sprintf("private name %s (declared in %s)", part, project)})
		}
	}
	for _, t := range r.terms {
		for _, m := range t.Re.FindAllString(line, -1) {
			if _, allowed := r.allow[m]; allowed || seen[m] {
				continue
			}
			seen[m] = true
			hits = append(hits, Hit{Text: m, Reason: fmt.Sprintf("private term (%s) %q", t.Kind, m)})
		}
	}
	return hits, nil
}
