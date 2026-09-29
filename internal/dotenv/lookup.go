package dotenv

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Lookup resolves keys from the process environment first, then from the
// project .env file, which is read only when a key is not in the environment.
type Lookup struct {
	path string
	file *File
	err  error
	read bool
}

// NewLookup resolves keys against the .env file at path.
func NewLookup(path string) *Lookup {
	return &Lookup{path: path}
}

// Value returns the value of key. A key found nowhere is an error: a command
// run with an empty password or DSN fails later and far less clearly.
func (l *Lookup) Value(key string) (string, error) {
	if v, ok := os.LookupEnv(key); ok {
		return v, nil
	}
	if !l.read {
		l.file, l.err = Load(l.path)
		l.read = true
	}
	if l.err != nil {
		if errors.Is(l.err, os.ErrNotExist) {
			return "", fmt.Errorf("%s is not set in the environment and %s does not exist", key, l.path)
		}
		return "", l.err
	}
	v, ok := l.file.Get(key)
	if !ok {
		return "", fmt.Errorf("%s is not set in the environment or in %s", key, l.path)
	}
	return v, nil
}

// Values resolves several keys into KEY=VALUE entries.
func (l *Lookup) Values(keys []string) ([]string, error) {
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := l.Value(k)
		if err != nil {
			return nil, err
		}
		env = append(env, k+"="+v)
	}
	return env, nil
}

// Expand replaces ${KEY} with the value of KEY and $$ with $. Any other $ is
// kept as is, so a literal $ needs no escaping unless it precedes {.
func Expand(s string, l *Lookup) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			key := s[i+2 : i+2+end]
			if !keyRe.MatchString(key) {
				return "", fmt.Errorf("invalid variable name %q in %q", key, s)
			}
			v, err := l.Value(key)
			if err != nil {
				return "", err
			}
			b.WriteString(v)
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// ExpandAll expands every element of argv.
func ExpandAll(argv []string, l *Lookup) ([]string, error) {
	out := make([]string, len(argv))
	for i, a := range argv {
		v, err := Expand(a, l)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
