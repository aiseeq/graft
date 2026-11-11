package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Commands outside the agent's common workflow stay discoverable in help.
var notForAgents = map[string]string{
	"amend":                     "rewrites a commit and needs a specific request",
	"release":                   "publishes a release and needs release-specific instructions",
	"version":                   "version management depends on project policy",
	"version bump":              "version management depends on project policy",
	"init":                      "installation is covered in the project setup",
	"check":                     "the commit workflow runs the staged checks",
	"gate":                      "the project's test tasks describe the gate",
	"hook":                      "called by git hooks",
	"hook pre-commit":           "called by git",
	"hook prepare-commit-msg":   "called by git",
	"hook post-merge":           "called by git",
	"hook pre-push":             "called by git",
	"flags":                     "event journal operation is project-specific",
	"flags status":              "event journal operation is project-specific",
	"flags show":                "event journal operation is project-specific",
	"flags ack":                 "event journal operation is project-specific",
	"flags mute":                "event journal operation is project-specific",
	"deploy status":             "target-specific operational inspection",
	"deploy logs":               "target-specific operational inspection",
	"deploy versions":           "target-specific operational inspection",
	"deploy cutoff":             "release-window operation",
	"deploy sweep":              "release-window operation",
	"deploy handover":           "release-window operation",
	"deploy check-head":         "intended for deploy scripts",
	"deploy check-integrations": "intended for deploy scripts",
	"run":                       "the task shortcut covers the same workflow",
	"start":                     "service operation is project-specific",
	"stop":                      "service operation is project-specific",
	"restart":                   "service operation is project-specific",
	"logs":                      "service operation is project-specific",
	"reap":                      "internal process management entry point",
	"status":                    "service and lock inspection is project-specific",
	"locks":                     "lock inspection is project-specific",
	"testdb":                    "test tasks manage their configured database",
	"testdb up":                 "test tasks manage their configured database",
	"testdb down":               "test tasks manage their configured database",
	"testdb status":             "test tasks manage their configured database",
	"testdb recreate":           "test tasks manage their configured database",
	"tools":                     "pinned tools are described by project configuration",
	"tools install":             "tool installation is project-specific",
	"gocache":                   "cache maintenance is a machine operation",
	"gocache trim":              "cache maintenance is a machine operation",
	"guide":                     "the reader is already reading the guide",
}

func TestRegistryGuideCoverage(t *testing.T) {
	entries := registryForTest(t)
	for _, e := range entries {
		path := strings.Join(e.Path, " ")
		mention := regexp.MustCompile(`\bgraft ` + regexp.QuoteMeta(path) + `(?:[^A-Za-z0-9_-]|$)`)
		if mention.MatchString(agentGuide) {
			continue
		}
		if notForAgents[path] == "" {
			t.Errorf("guide does not mention %s and notForAgents has no reason", path)
		}
	}
	for path, reason := range notForAgents {
		if reason == "" || entryFor(entries, path) == nil {
			t.Errorf("invalid notForAgents entry %q: %q", path, reason)
		}
	}
}

var (
	inlineCode = regexp.MustCompile("`([^`]+)`")
	graftWord  = regexp.MustCompile(`\bgraft[ \t]+`)
	docToken   = regexp.MustCompile(`--[A-Za-z0-9][A-Za-z0-9-]*(?:=[^\s\]\[|]+)?|-[A-Za-z][A-Za-z0-9-]*(?:=[^\s\]\[|]+)?|--|<[^>]*>|[A-Za-z0-9_./{}-]+`)
)

// docExamples yields inline spans and lines of fenced blocks. A fenced line
// may contain a shell assignment or command before graft.
func docExamples(source string) []string {
	var examples []string
	var prose strings.Builder
	flush := func() {
		for _, match := range inlineCode.FindAllStringSubmatch(prose.String(), -1) {
			examples = append(examples, match[1])
		}
		prose.Reset()
	}
	fenced := false
	for line := range strings.SplitSeq(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if !fenced {
				flush()
			}
			fenced = !fenced
			continue
		}
		if fenced {
			examples = append(examples, line)
		} else {
			prose.WriteString(line)
			prose.WriteByte('\n')
		}
	}
	flush()
	return examples
}

func checkDocExample(t *testing.T, entries []registryCommand, source, example string) {
	t.Helper()
	for _, start := range graftWord.FindAllStringIndex(example, -1) {
		rest := example[start[1]:]
		// A shell command separator ends this invocation.
		if i := strings.Index(rest, " && "); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.Index(rest, ";"); i >= 0 {
			rest = rest[:i]
		}
		tokens := docToken.FindAllString(rest, -1)
		if len(tokens) == 0 {
			continue
		}
		for len(tokens) >= 2 && tokens[0] == "-C" {
			tokens = tokens[2:]
		}
		if len(tokens) == 0 || (strings.HasPrefix(tokens[0], "<") && tokens[0] != "<task>") || tokens[0] == "--version" || tokens[0] == "-v" || tokens[0] == "--help" || tokens[0] == "-h" {
			continue
		}
		if strings.HasPrefix(tokens[0], "-") {
			t.Errorf("%s: graft: unknown command or flag %s", source, tokens[0])
			continue
		}
		var command *registryCommand
		used := 0
		for _, e := range entries {
			if len(e.Path) <= len(tokens) && len(e.Path) > used && slices.Equal(e.Path, tokens[:len(e.Path)]) {
				copyEntry := e
				command, used = &copyEntry, len(e.Path)
			}
		}
		if command == nil {
			// The first word names a task. Generic placeholders have no known
			// command, but explicit task names still allow only --no-cache.
			used = 1
		}
		for i := used; i < len(tokens); i++ {
			name, _, hasValue := strings.Cut(tokens[i], "=")
			if name == "--" {
				if command == nil || command.Rest == "after-dashdash" {
					break
				}
				continue
			}
			if !strings.HasPrefix(name, "-") {
				continue
			}
			if name == "-h" || name == "--help" {
				continue // every command answers them with its help
			}
			if command == nil {
				// graft <task> takes only --no-cache before --.
				if name != "--no-cache" {
					t.Errorf("%s: graft %s: unknown flag %s", source, strings.Join(tokens[:used], " "), name)
				}
				continue
			}
			if !slices.Contains(command.Flags, name) {
				t.Errorf("%s: graft %s: unknown flag %s", source, strings.Join(tokens[:used], " "), name)
				continue
			}
			if !hasValue && slices.Contains(command.ValueFlags, name) && i+1 < len(tokens) {
				i++
			}
		}
	}
}

func TestRegistryDocumentation(t *testing.T) {
	entries := registryForTest(t)
	for _, path := range []string{"docs/agents.md", "README.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, example := range docExamples(string(data)) {
			checkDocExample(t, entries, path, example)
		}
	}
}
