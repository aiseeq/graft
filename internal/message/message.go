// Package message reads a commit message from the user and adds the work item
// key taken from the branch name.
package message

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Source says where the message comes from: exactly one of Parts and File is
// set. Standard input is read only when asked for with File "-": an implicit
// read would hang a terminal that only looks like a pipe (mintty on Windows).
type Source struct {
	// Parts are -m values; each becomes a paragraph, as with git commit -m.
	Parts []string
	// File is a -F value; "-" means standard input.
	File string
	// Stdin is the process standard input.
	Stdin io.Reader
}

// Read returns the message verbatim apart from line endings (CRLF becomes LF)
// and surrounding blank space. An empty message is an error: there is no
// default subject that would describe somebody else's change.
func Read(src Source) (string, error) {
	var raw string
	switch {
	case len(src.Parts) > 0 && src.File != "":
		return "", errors.New("use either -m or -F, not both")
	case len(src.Parts) > 0:
		raw = strings.Join(src.Parts, "\n\n")
	case src.File == "-":
		data, err := io.ReadAll(src.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading message from stdin: %w", err)
		}
		raw = string(data)
	case src.File != "":
		data, err := os.ReadFile(src.File)
		if err != nil {
			return "", fmt.Errorf("reading message file: %w", err)
		}
		raw = string(data)
	default:
		return "", errors.New("no commit message: pass -m \"...\", -F <file>, or -F - to read it from stdin")
	}
	msg := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if msg == "" {
		return "", errors.New("commit message is empty")
	}
	return msg, nil
}

// conventional is a Conventional Commits subject: a type of the common set,
// an optional scope, ! for a breaking change, then the description.
var conventional = regexp.MustCompile(`^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([^()\n]+\))?!?: \S`)

// ConventionalType returns the Conventional Commits type of the subject, ""
// for a subject that is not one.
func ConventionalType(subject string) string {
	if m := conventional.FindStringSubmatch(subject); m != nil {
		return m[1]
	}
	return ""
}

// CheckConventional refuses a message whose subject is not a Conventional
// Commits one (type(scope)!: description); git's own revert subject, Revert
// "...", passes.
func CheckConventional(msg string) error {
	subject, _, _ := strings.Cut(msg, "\n")
	if conventional.MatchString(subject) || strings.HasPrefix(subject, `Revert "`) {
		return nil
	}
	return fmt.Errorf("the subject %q is not Conventional Commits (user config commit.conventional): type(scope): description, the type one of build, chore, ci, docs, feat, fix, perf, refactor, revert, style, test", subject)
}

// BranchKey returns the first work item key in the branch name, or "" when the
// branch names none.
func BranchKey(branch string, pattern *regexp.Regexp) string {
	if pattern == nil {
		return ""
	}
	return pattern.FindString(branch)
}

// WordKeys returns the keys matching pattern in text, in order, each filling a
// word once punctuation is trimmed from its ends. Words are split by spaces,
// brackets and the marks around a key in prose or a scope: fix(PROJ-1): x.
func WordKeys(text string, pattern *regexp.Regexp) []string {
	if pattern == nil {
		return nil
	}
	var keys []string
	for word := range strings.FieldsFuncSeq(text, wordSeparator) {
		if key := wordKey(word, pattern); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// wordKey returns the part of word, punctuation trimmed from its ends in every
// amount, that the pattern matches whole, or "" when none does.
func wordKey(word string, pattern *regexp.Regexp) string {
	starts := []int{0}
	for i, r := range word {
		if !ticketPunctuation(r) {
			break
		}
		starts = append(starts, i+utf8.RuneLen(r))
	}
	ends := []int{len(word)}
	for i := len(word); i > 0; {
		r, size := utf8.DecodeLastRuneInString(word[:i])
		if !ticketPunctuation(r) {
			break
		}
		i -= size
		ends = append(ends, i)
	}
	for _, start := range starts {
		for _, end := range ends {
			if start >= end {
				continue
			}
			candidate := word[start:end]
			if loc := pattern.FindStringIndex(candidate); loc != nil && loc[0] == 0 && loc[1] == len(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func wordSeparator(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("()[]{}<>:;,\"'`", r)
}

func ticketPunctuation(r rune) bool { return r != '_' && unicode.IsPunct(r) }

// WithKey returns msg with key added as a paragraph of its own, unless the
// message already mentions it. The key goes before a closing trailer block
// (Co-Authored-By: ...) so git still recognises the trailers.
func WithKey(msg, key string) string {
	msg = strings.TrimSpace(msg)
	if key == "" || mentions(msg, key) {
		return msg + "\n"
	}
	paragraphs := strings.Split(msg, "\n\n")
	last := len(paragraphs) - 1
	if last > 0 && isTrailerBlock(paragraphs[last]) {
		return strings.Join(paragraphs[:last], "\n\n") + "\n\n" + key + "\n\n" + paragraphs[last] + "\n"
	}
	return msg + "\n\n" + key + "\n"
}

// mentions reports whether key occurs in msg as a whole key: PROJ-12 is not
// mentioned by PROJ-123 or XPROJ-12.
func mentions(msg, key string) bool {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(key) + `($|[^0-9])`)
	return re.MatchString(msg)
}

var trailerLine = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*:\s`)

// isTrailerBlock reports whether every line of the paragraph is a git trailer
// ("Token: value") or its indented continuation.
func isTrailerBlock(paragraph string) bool {
	lines := strings.Split(paragraph, "\n")
	if !trailerLine.MatchString(lines[0]) {
		return false
	}
	for _, line := range lines[1:] {
		if !trailerLine.MatchString(line) && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			return false
		}
	}
	return true
}
