// Package dotenv reads and writes single keys of a .env file without ever
// sourcing it. One parser serves every reader, so a value means the same
// thing to every command that asks for it.
//
// Accepted lines: KEY=value, export KEY=value, blank lines and lines starting
// with #. A value is taken as is to the end of the line (trailing blanks
// trimmed), or quoted: '...' literally, "..." with \" \\ \n \$ escapes. A #
// after an unquoted value is part of the value. Anything else is an error that
// names the line.
package dotenv

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// File is a parsed .env file. When a key repeats, the last value wins.
type File struct {
	Path   string
	values map[string]string
}

// Load parses the file at path.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		key, value, ok, err := parseLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if ok {
			values[key] = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &File{Path: path, values: values}, nil
}

// Get returns the value of key and whether the file defines it.
func (f *File) Get(key string) (string, bool) {
	v, ok := f.values[key]
	return v, ok
}

// parseLine returns ok=false for blank and comment lines.
func parseLine(line string) (key, value string, ok bool, err error) {
	trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false, nil
	}
	trimmed = strings.TrimPrefix(trimmed, "export ")
	name, raw, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", false, fmt.Errorf("expected KEY=value, got %q", trimmed)
	}
	name = strings.TrimSpace(name)
	if !keyRe.MatchString(name) {
		return "", "", false, fmt.Errorf("invalid key %q", name)
	}
	value, err = parseValue(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false, fmt.Errorf("%s: %w", name, err)
	}
	return name, value, true, nil
}

func parseValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		end := strings.IndexByte(raw[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single quote")
		}
		if rest := strings.TrimSpace(raw[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", fmt.Errorf("text after the closing quote: %q", rest)
		}
		return raw[1 : end+1], nil
	case '"':
		return parseDoubleQuoted(raw)
	default:
		return raw, nil
	}
}

func parseDoubleQuoted(raw string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\\':
			if i+1 >= len(raw) {
				return "", errors.New("unterminated double quote")
			}
			i++
			switch raw[i] {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\', '$', '`':
				b.WriteByte(raw[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(raw[i])
			}
		case '"':
			if rest := strings.TrimSpace(raw[i+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
				return "", fmt.Errorf("text after the closing quote: %q", rest)
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated double quote")
}
