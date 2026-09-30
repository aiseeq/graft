package version

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aiseeq/graft/internal/config"
)

// Target is a file that holds the version: the version file itself or one of
// the files synchronised with it.
type Target struct {
	// Path is slash-separated and relative to the work tree root.
	Path string
	sync config.SyncFile
}

// Targets lists the version file first, then the synchronised files.
func Targets(v config.Version) []Target {
	targets := []Target{{Path: v.File, sync: config.SyncFile{Path: v.File, Format: config.FormatPlain}}}
	for _, s := range v.Sync {
		targets = append(targets, Target{Path: s.Path, sync: s})
	}
	return targets
}

type span struct{ start, end int }

// Extract returns the version recorded in content. Every occurrence the target
// describes must carry the same value.
func (t Target) Extract(content []byte) (string, error) {
	spans, err := t.locate(content)
	if err != nil {
		return "", err
	}
	value := string(content[spans[0].start:spans[0].end])
	for _, s := range spans[1:] {
		if other := string(content[s.start:s.end]); other != value {
			return "", fmt.Errorf("%s: disagrees with itself: %q and %q", t.Path, value, other)
		}
	}
	return value, nil
}

// Replace returns content with every occurrence of the version set to v.
func (t Target) Replace(content []byte, v string) ([]byte, error) {
	spans, err := t.locate(content)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	prev := 0
	for _, s := range spans {
		out.Write(content[prev:s.start])
		out.WriteString(v)
		prev = s.end
	}
	out.Write(content[prev:])
	return out.Bytes(), nil
}

func (t Target) locate(content []byte) ([]span, error) {
	var (
		spans []span
		err   error
	)
	switch t.sync.Format {
	case config.FormatPlain:
		spans, err = locatePlain(content)
	case config.FormatJSON:
		spans, err = locateJSON(content, t.sync.Key)
	case config.FormatRegex:
		spans, err = locateRegex(content, t.sync)
	default:
		err = fmt.Errorf("unknown format %q", t.sync.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Path, err)
	}
	return spans, nil
}

func locatePlain(content []byte) ([]span, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, errors.New("file is empty")
	}
	start := bytes.Index(content, trimmed)
	return []span{{start, start + len(trimmed)}}, nil
}

func locateRegex(content []byte, s config.SyncFile) ([]span, error) {
	matches := s.Regexp.FindAllSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("pattern %q matches nothing", s.Pattern)
	}
	spans := make([]span, 0, len(matches))
	for _, m := range matches {
		if m[2] < 0 {
			return nil, fmt.Errorf("pattern %q matched without its capture group", s.Pattern)
		}
		spans = append(spans, span{m[2], m[3]})
	}
	return spans, nil
}

// locateJSON finds the string value at the object key path without re-encoding
// the document, so formatting and key order survive a bump. It scans the bytes
// itself: encoding/json's Decoder.InputOffset drifts after escaped strings and
// cannot be trusted to locate anything.
func locateJSON(content []byte, key []string) ([]span, error) {
	s := &jsonScanner{data: content, key: key}
	s.skipSpace()
	if err := s.value([]string{}, true); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	s.skipSpace()
	if s.pos != len(s.data) {
		return nil, fmt.Errorf("invalid JSON: trailing data at byte %d", s.pos)
	}
	name := strings.Join(key, ".")
	switch len(s.found) {
	case 0:
		return nil, fmt.Errorf("key %q not found", name)
	case 1:
		return s.found, nil
	default:
		return nil, fmt.Errorf("key %q occurs %d times", name, len(s.found))
	}
}

type jsonScanner struct {
	data  []byte
	pos   int
	key   []string
	found []span
}

func (s *jsonScanner) skipSpace() {
	for s.pos < len(s.data) && strings.IndexByte(" \t\r\n", s.data[s.pos]) >= 0 {
		s.pos++
	}
}

func (s *jsonScanner) expect(b byte) error {
	s.skipSpace()
	if s.pos >= len(s.data) || s.data[s.pos] != b {
		return fmt.Errorf("expected %q at byte %d", b, s.pos)
	}
	s.pos++
	return nil
}

// value consumes one JSON value. onPath is true while the value sits on the
// key path being searched; everything off the path is skipped whole, so a
// nested object with a same-named key is never mistaken for the target.
func (s *jsonScanner) value(path []string, onPath bool) error {
	s.skipSpace()
	if s.pos >= len(s.data) {
		return errors.New("unexpected end of input")
	}
	switch c := s.data[s.pos]; {
	case c == '{':
		s.pos++
		return s.object(path, onPath)
	case c == '[':
		s.pos++
		return s.array()
	case c == '"':
		_, _, err := s.str()
		return err
	default:
		return s.scalar()
	}
}

func (s *jsonScanner) object(path []string, onPath bool) error {
	s.skipSpace()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++
		return nil
	}
	for {
		s.skipSpace()
		name, _, err := s.str()
		if err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		child := append(slices.Clone(path), name)
		switch {
		case onPath && slices.Equal(child, s.key):
			err = s.target()
		case onPath && isPrefix(child, s.key):
			err = s.value(child, true)
		default:
			err = s.value(nil, false)
		}
		if err != nil {
			return err
		}
		s.skipSpace()
		if s.pos < len(s.data) && s.data[s.pos] == ',' {
			s.pos++
			continue
		}
		return s.expect('}')
	}
}

func (s *jsonScanner) array() error {
	s.skipSpace()
	if s.pos < len(s.data) && s.data[s.pos] == ']' {
		s.pos++
		return nil
	}
	for {
		if err := s.value(nil, false); err != nil {
			return err
		}
		s.skipSpace()
		if s.pos < len(s.data) && s.data[s.pos] == ',' {
			s.pos++
			continue
		}
		return s.expect(']')
	}
}

// target records the span of the string value the key path points at.
func (s *jsonScanner) target() error {
	s.skipSpace()
	name := strings.Join(s.key, ".")
	if s.pos >= len(s.data) || s.data[s.pos] != '"' {
		return fmt.Errorf("key %q does not hold a string", name)
	}
	_, raw, err := s.str()
	if err != nil {
		return err
	}
	if strings.Contains(raw, "\\") {
		return fmt.Errorf("key %q: escaped string %s is not supported", name, raw)
	}
	s.found = append(s.found, span{s.pos - len(raw) + 1, s.pos - 1})
	return nil
}

// str consumes a string literal and returns its decoded value and raw text.
func (s *jsonScanner) str() (string, string, error) {
	if s.pos >= len(s.data) || s.data[s.pos] != '"' {
		return "", "", fmt.Errorf("expected a string at byte %d", s.pos)
	}
	start := s.pos
	for s.pos++; s.pos < len(s.data); s.pos++ {
		switch s.data[s.pos] {
		case '\\':
			s.pos++
		case '"':
			s.pos++
			raw := string(s.data[start:s.pos])
			var decoded string
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				return "", "", fmt.Errorf("string at byte %d: %w", start, err)
			}
			return decoded, raw, nil
		}
	}
	return "", "", fmt.Errorf("unterminated string at byte %d", start)
}

// scalar consumes a number, true, false or null.
func (s *jsonScanner) scalar() error {
	start := s.pos
	for s.pos < len(s.data) && strings.IndexByte(",}] \t\r\n", s.data[s.pos]) < 0 {
		s.pos++
	}
	var v any
	if start == s.pos || json.Unmarshal(s.data[start:s.pos], &v) != nil {
		return fmt.Errorf("invalid value at byte %d", start)
	}
	return nil
}

func isPrefix(prefix, full []string) bool {
	return len(prefix) < len(full) && slices.Equal(prefix, full[:len(prefix)])
}

// Backup holds the original content of files a bump rewrote.
type Backup struct {
	root  string
	files []savedFile
}

type savedFile struct {
	path    string
	content []byte
}

// Paths lists the files the backup covers.
func (b *Backup) Paths() []string {
	paths := make([]string, 0, len(b.files))
	for _, f := range b.files {
		paths = append(paths, f.path)
	}
	return paths
}

// Restore writes the original content back.
func (b *Backup) Restore() error {
	var errs []error
	for _, f := range b.files {
		if err := writeKeepingMode(filepath.Join(b.root, filepath.FromSlash(f.path)), f.content); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Current reads the version from the version file in the work tree.
func Current(root string, v config.Version) (Semver, error) {
	target := Targets(v)[0]
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.Path)))
	if err != nil {
		return Semver{}, fmt.Errorf("reading the version file: %w", err)
	}
	raw, err := target.Extract(content)
	if err != nil {
		return Semver{}, err
	}
	sv, err := Parse(raw)
	if err != nil {
		return Semver{}, fmt.Errorf("%s: %w", target.Path, err)
	}
	return sv, nil
}

// Write sets every target in the work tree to next. Nothing is written unless
// every target can be rewritten; a failed write restores the files already
// written.
func Write(root string, v config.Version, next Semver) (*Backup, error) {
	type pending struct {
		path     string
		original []byte
		updated  []byte
	}
	// Several targets may share a file (package-lock.json keeps the version
	// twice): each file is read once, every target's replacement applies to
	// the same buffer in turn, and the file is written once.
	var plan []*pending
	byPath := map[string]*pending{}
	for _, t := range Targets(v) {
		p, ok := byPath[t.Path]
		if !ok {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.Path)))
			if err != nil {
				return nil, fmt.Errorf("reading a version target: %w", err)
			}
			p = &pending{path: t.Path, original: content, updated: content}
			byPath[t.Path] = p
			plan = append(plan, p)
		}
		updated, err := t.Replace(p.updated, next.String())
		if err != nil {
			return nil, err
		}
		p.updated = updated
	}
	backup := &Backup{root: root}
	for _, p := range plan {
		if err := writeKeepingMode(filepath.Join(root, filepath.FromSlash(p.path)), p.updated); err != nil {
			if restoreErr := backup.Restore(); restoreErr != nil {
				return nil, errors.Join(err, fmt.Errorf("restoring version files: %w", restoreErr))
			}
			return nil, err
		}
		backup.files = append(backup.files, savedFile{p.path, p.original})
	}
	return backup, nil
}

// writeKeepingMode rewrites an existing file, lifting and restoring a
// read-only bit: generated files are often read-only to discourage hand edits,
// and the bump is the one writer that must get through.
func writeKeepingMode(path string, content []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("rewriting a version target: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode|0o200); err != nil {
			return fmt.Errorf("making a version target writable: %w", err)
		}
	}
	writeErr := os.WriteFile(path, content, mode)
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode); err != nil {
			return errors.Join(writeErr, err)
		}
	}
	return writeErr
}
