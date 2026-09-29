package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Command is one gate step: a program and its arguments, run without a shell.
type Command struct {
	// Argv is the program followed by its arguments.
	Argv []string
	// Source is the command as written in the config, for messages.
	Source string
}

// UnmarshalYAML accepts either a string, split into words by shell quoting
// rules, or a list taken as argv verbatim.
func (c *Command) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		argv, err := SplitCommand(node.Value)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		c.Argv, c.Source = argv, node.Value
		return nil
	case yaml.SequenceNode:
		var argv []string
		if err := node.Decode(&argv); err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		if len(argv) == 0 || argv[0] == "" {
			return fmt.Errorf("line %d: empty command", node.Line)
		}
		c.Argv, c.Source = argv, strings.Join(argv, " ")
		return nil
	default:
		return fmt.Errorf("line %d: a command is a string or a list of strings", node.Line)
	}
}

var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

const shellHint = "commands run without a shell; write [sh, -c, '...'] or move the logic into a script"

// SplitCommand splits s into words the way a POSIX shell would for plain
// words, single quotes, double quotes and backslash escapes. Anything that
// would make a shell do more than split words (pipes, redirects, expansions,
// globs, variable assignments) is an error: without a shell it would silently
// mean something else.
func SplitCommand(s string) ([]string, error) {
	var (
		words   []string
		current strings.Builder
		inWord  bool
	)
	flush := func() {
		if inWord {
			words = append(words, current.String())
			current.Reset()
			inWord = false
		}
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		case r == '\'':
			end := indexRune(runes, i+1, '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote in %q", s)
			}
			current.WriteString(string(runes[i+1 : end]))
			inWord = true
			i = end
		case r == '"':
			end, err := readDoubleQuoted(runes, i+1, &current, s)
			if err != nil {
				return nil, err
			}
			inWord = true
			i = end
		case r == '\\':
			if i+1 >= len(runes) {
				return nil, fmt.Errorf("trailing backslash in %q", s)
			}
			current.WriteRune(runes[i+1])
			inWord = true
			i++
		case strings.ContainsRune("|&;<>()`$", r):
			return nil, fmt.Errorf("shell syntax %q in %q: %s", string(r), s, shellHint)
		case strings.ContainsRune("*?[", r):
			return nil, fmt.Errorf("glob %q in %q is not expanded: quote it or %s", string(r), s, shellHint)
		case r == '~' && !inWord:
			return nil, fmt.Errorf("~ in %q is not expanded: %s", s, shellHint)
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	flush()
	if len(words) == 0 {
		return nil, errors.New("empty command")
	}
	if envAssignment.MatchString(words[0]) {
		return nil, fmt.Errorf("variable assignment %q in %q: %s", words[0], s, shellHint)
	}
	return words, nil
}

func indexRune(runes []rune, from int, target rune) int {
	for i := from; i < len(runes); i++ {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

// readDoubleQuoted consumes a double-quoted string starting after the opening
// quote and returns the index of the closing quote.
func readDoubleQuoted(runes []rune, from int, out *strings.Builder, source string) (int, error) {
	for i := from; i < len(runes); i++ {
		switch runes[i] {
		case '"':
			return i, nil
		case '\\':
			if i+1 < len(runes) && strings.ContainsRune("\"\\$`", runes[i+1]) {
				out.WriteRune(runes[i+1])
				i++
				continue
			}
			out.WriteRune('\\')
		case '$', '`':
			return 0, fmt.Errorf("shell expansion %q in %q: %s", string(runes[i]), source, shellHint)
		default:
			out.WriteRune(runes[i])
		}
	}
	return 0, fmt.Errorf("unterminated double quote in %q", source)
}
