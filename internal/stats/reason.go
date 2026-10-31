package stats

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

// LimitReason removes terminal escapes and bounds a reason for journal lines.
func LimitReason(text string) string {
	return cleanReason(text, false)
}

// CleanReason removes graft chatter and warnings before bounding a failed
// step's captured output.
func CleanReason(text string) string {
	return cleanReason(text, true)
}

func cleanReason(text string, filter bool) string {
	text = ansiEscape.ReplaceAllString(strings.ReplaceAll(text, "\r\n", "\n"), "")
	text = strings.ReplaceAll(text, "\r", "\n")
	var kept []string
	total := 0
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if filter && (strings.HasPrefix(line, "graft:") || strings.HasPrefix(line, "warning:")) {
			continue
		}
		if line == "" || len(kept) == 10 || total >= 2000 {
			continue
		}
		runes := []rune(line)
		if len(runes) > 300 {
			line = string(runes[:299]) + "…"
		}
		left := 2000 - total
		if len(kept) > 0 {
			left--
		}
		if utf8.RuneCountInString(line) > left {
			line = string([]rune(line)[:left])
		}
		if len(kept) > 0 {
			total++
		}
		kept = append(kept, line)
		total += utf8.RuneCountInString(line)
	}
	return strings.Join(kept, "\n")
}
