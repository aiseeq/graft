// Package jsonspan locates string values in JSON without re-encoding the
// document. Its byte offsets remain correct when earlier strings contain
// escapes, unlike encoding/json.Decoder.InputOffset.
package jsonspan

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Span is the half-open byte range inside a JSON string's quotation marks.
type Span struct{ Start, End int }

// Locate finds the string value at key's object path. It requires exactly one
// matching key path and does not confuse equal keys in unrelated subobjects.
func Locate(content []byte, key []string) ([]Span, error) {
	s := &scanner{data: content, key: key}
	s.skipSpace()
	if err := s.value(nil, true); err != nil {
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

type scanner struct {
	data  []byte
	pos   int
	key   []string
	found []Span
}

func (s *scanner) skipSpace() {
	for s.pos < len(s.data) && strings.IndexByte(" \t\r\n", s.data[s.pos]) >= 0 {
		s.pos++
	}
}

func (s *scanner) expect(b byte) error {
	s.skipSpace()
	if s.pos >= len(s.data) || s.data[s.pos] != b {
		return fmt.Errorf("expected %q at byte %d", b, s.pos)
	}
	s.pos++
	return nil
}

func (s *scanner) value(path []string, onPath bool) error {
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

func (s *scanner) object(path []string, onPath bool) error {
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

func (s *scanner) array() error {
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

func (s *scanner) target() error {
	s.skipSpace()
	name := strings.Join(s.key, ".")
	if s.pos >= len(s.data) || s.data[s.pos] != '"' {
		return fmt.Errorf("key %q does not hold a string", name)
	}
	_, raw, err := s.str()
	if err != nil {
		return err
	}
	s.found = append(s.found, Span{s.pos - len(raw) + 1, s.pos - 1})
	return nil
}

func (s *scanner) str() (string, string, error) {
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

func (s *scanner) scalar() error {
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
