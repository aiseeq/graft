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
	// over and parent make a layered lookup (With).
	over   map[string]string
	parent *Lookup
}

// With returns a lookup that resolves the keys in vars first and everything
// else as l does, sharing l's reading of the file.
func (l *Lookup) With(vars map[string]string) *Lookup {
	return &Lookup{over: vars, parent: l}
}

// NewLookup resolves keys against the .env file at path.
func NewLookup(path string) *Lookup {
	return &Lookup{path: path}
}

// Value returns the value of key. A key found nowhere is an error: a command
// run with an empty password or DSN fails later and far less clearly.
func (l *Lookup) Value(key string) (string, error) {
	if l.parent != nil {
		if v, ok := l.over[key]; ok {
			return v, nil
		}
		return l.parent.Value(key)
	}
	if v, ok := os.LookupEnv(key); ok {
		return v, nil
	}
	if !l.read {
		l.file, l.err = Load(l.path)
		l.read = true
	}
	if l.err != nil {
		if errors.Is(l.err, os.ErrNotExist) {
			return "", fmt.Errorf("%s is %w in the environment and %s does not exist", key, ErrNotSet, l.path)
		}
		return "", l.err
	}
	v, ok := l.file.Get(key)
	if !ok {
		return "", fmt.Errorf("%s is %w in the environment or in %s", key, ErrNotSet, l.path)
	}
	return v, nil
}

// ErrNotSet means a key is neither in the environment nor in the .env file.
var ErrNotSet = errors.New("not set")

// Optional returns the value of key and whether it is set anywhere; only an
// unreadable or malformed .env file is an error.
func (l *Lookup) Optional(key string) (string, bool, error) {
	v, err := l.Value(key)
	if errors.Is(err, ErrNotSet) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
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
	return expand(s, l.Value)
}

// Refs lists the keys s refers to as ${KEY}.
func Refs(s string) ([]string, error) {
	var keys []string
	_, err := expand(s, func(key string) (string, error) {
		keys = append(keys, key)
		return "", nil
	})
	return keys, err
}

func expand(s string, resolve func(string) (string, error)) (string, error) {
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
			v, err := resolve(key)
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
