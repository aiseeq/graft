package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/aiseeq/graft/v2/internal/config"
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
	"hook global-pre-commit":    "called by git",
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
	"tools install-dir":         "prints the pinned tool installation directory",
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

func TestREADMEDeployPriorityOrder(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(readme)
	if !strings.Contains(s, "from graft deploy use the same FIFO queue and move ahead of ordinary queued\ntasks. The queue head that waits for the machine does not hold `load.lock`\nwhile it waits") {
		t.Fatal("README must explain that deploy moves ahead of ordinary queued tasks and that a holder waiting for the machine gives load.lock up")
	}
	if strings.Contains(s, "cannot pass an earlier waiter") {
		t.Fatal("README still contradicts deploy priority over ordinary queued tasks")
	}
}

var (
	inlineCode = regexp.MustCompile("`([^`]+)`")
	graftWord  = regexp.MustCompile(`\bgraft[ \t]+`)
	docToken   = regexp.MustCompile(`--%[A-Za-z]|--[A-Za-z0-9][A-Za-z0-9-]*(?:=[^\s\]\[|]+)?|-[A-Za-z][A-Za-z0-9-]*(?:=[^\s\]\[|]+)?|%[A-Za-z]|--|<[^>]*>|[A-Za-z0-9_./{}-]+`)
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
	checkDocExampleMode(t, entries, source, example, false, nil)
}

func checkDocExampleMode(t *testing.T, entries []registryCommand, source, example string, ignoreUnknown bool, projectTasks map[string]bool) {
	t.Helper()
	for _, start := range graftWord.FindAllStringIndex(example, -1) {
		rest := example[start[1]:]
		if strings.HasPrefix(rest, "--- ") {
			continue // decorative separator in log output
		}
		// Shell and prose separators end this invocation.
		if i := strings.Index(rest, " && "); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.Index(rest, ";"); i >= 0 {
			rest = rest[:i]
		}
		for i, r := range rest {
			if r == '\n' || r == ',' || r == ')' || r == ':' || r == '`' ||
				(r == '.' && (i+1 == len(rest) || rest[i+1] == ' ' || rest[i+1] == '\n')) {
				rest = rest[:i]
				break
			}
		}
		tokens := docToken.FindAllString(rest, -1)
		if len(tokens) == 0 {
			continue
		}
		if ignoreUnknown && tokens[0] == "s" && strings.Contains(rest, "%s") {
			continue // a format placeholder was split at its percent sign
		}
		for len(tokens) >= 2 && tokens[0] == "-C" {
			tokens = tokens[2:]
		}
		if len(tokens) == 0 || (strings.HasPrefix(tokens[0], "<") && tokens[0] != "<task>") || tokens[0] == "--version" || tokens[0] == "-v" || tokens[0] == "--help" || tokens[0] == "-h" {
			continue
		}
		if ignoreUnknown && strings.HasPrefix(tokens[0], "%") {
			continue // a format substitution supplies the command path
		}
		if strings.HasPrefix(tokens[0], "-") {
			if tokens[0] == "-C" {
				continue // global option, before any command path
			}
			if ignoreUnknown && tokens[0] == "-" {
				continue // prose separator, before any command path
			}
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
			if ignoreUnknown && len(tokens) > 1 && tokens[1] == "--" {
				if nearBuiltinCommand(tokens[0], entries) && !projectTasks[tokens[0]] {
					t.Errorf("%s: graft: unknown command %s before -- (one edit from a built-in command)", source, tokens[0])
					continue
				}
				// A task name comes from project configuration, not the command
				// registry. Its graft-owned options end at the argument separator.
				used = 1
			} else if ignoreUnknown {
				// In Go literals, an unknown path-shaped first word is an
				// attempted command even if prose follows it.
				if pathWord(tokens[0]) {
					t.Errorf("%s: graft: unknown command %s in %q", source, tokens[0], rest)
				}
				continue
			}
			// The first word names a task. Generic placeholders have no known
			// command, but explicit task names still allow only --no-cache.
			used = 1
		}
		for i := used; i < len(tokens); i++ {
			name, _, hasValue := strings.Cut(tokens[i], "=")
			if ignoreUnknown && name == "-" {
				continue // decorative separator in log output
			}
			if name == "--" {
				if command == nil || command.Rest == "after-dashdash" {
					break
				}
				continue
			}
			if !strings.HasPrefix(name, "-") {
				if command == nil {
					continue
				}
				if command.Rest == "none" {
					hasChild := slices.ContainsFunc(entries, func(e registryCommand) bool {
						return len(e.Path) > len(command.Path) && slices.Equal(e.Path[:len(command.Path)], command.Path)
					})
					hasLaterFlag := slices.ContainsFunc(tokens[i+1:], func(token string) bool { return strings.HasPrefix(token, "-") })
					valueAlreadySeen := slices.ContainsFunc(tokens[:i], func(token string) bool {
						name, _, _ := strings.Cut(token, "=")
						return slices.Contains(command.ValueFlags, name)
					})
					if hasLaterFlag || (hasChild && pathWord(tokens[i]) && !valueAlreadySeen) {
						t.Errorf("%s: graft %s: unexpected word %s in %q", source, strings.Join(command.Path, " "), tokens[i], rest)
					}
					break // a prose continuation, not a command argument
				}
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
			if !registryHasFlag(command, name) {
				t.Errorf("%s: graft %s: unknown flag %s", source, strings.Join(tokens[:used], " "), name)
				continue
			}
			if !hasValue && slices.Contains(command.ValueFlags, name) && i+1 < len(tokens) {
				i++
			}
		}
	}
}

var pathWordPattern = regexp.MustCompile(`^[a-z][a-z-]*$`)

// Each exception identifies a unique span in one non-test Go string literal.
// The test fails if the source literal changes or disappears.
type goHintException struct {
	file, literal, reason string
}

var goHintExceptions = []goHintException{
	{"help.go", "  125  graft itself: a usage error, -C, the config, the queue, remote_heavy's run that does not start, an interrupt that ended the command, a command that exited 0 while the kernel killed processes of its heavy scope at a memory limit\n", "exit-status description, not a command"},
	{"help.go", "  125  graft itself: not Linux, no subreaper\n", "exit-status description, not a command"},
	{"help.go", "this text; one command's part of it; the version of graft itself", "help summary refers to the program itself"},
	{"internal/hooks/hooks.go", "graft itself", "prose refers to the program implementing the hook"},
	{"internal/app/handover.go", "graft moves ", "lock description for a handover operation"},
	{"internal/testdb/source.go", "graft used ", "database source marker, not a command hint"},
	{"internal/app/stats_repo.go", "locating the graft worktree cache: %w", "error description of the cache location"},
	{"internal/app/stats_repo.go", "resolving graft worktree cache %s: %w", "error description of the cache path"},
	{"internal/app/stats_repo.go", "finding project %s relative to graft worktree cache %s: %w", "error description of the cache relationship"},
	{"help.go", "graft deploys on this machine and names their project and target while waiting", "prose describing deploy lock behavior"},
	{"help.go", "A graft the command starts gets it too", "prose describing inherited memory limits"},
	{"help.go", "graft takes the command along", "prose describing signal handling"},
	{"help.go", "graft with the same signal", "prose describing exit status"},
	{"internal/app/hook_pre_push.go", "graft scans commits and tag messages only", "prose describing hook scope"},
	{"internal/app/worktree.go", "graft file directory", "error text describing a temporary file directory"},
	{"internal/app/worktree_sync.go", "graft compares the worktree branch with it", "prose describing a branch requirement"},
	{"internal/app/worktree_sync.go", "not a remote branch: graft pushes a worktree branch only to its own name", "prose describing a non-branch upstream"},
	{"internal/app/worktree_sync.go", "not %s/%s: graft pushes a worktree branch only to its own name", "prose describing an upstream on the wrong remote"},
	{"internal/config/deploy.go", "graft always counts a commit for the items it is the work of", "prose explaining a removed configuration key"},
	{"internal/config/tasks.go", "is a graft command, name the task otherwise", "error text explaining task naming"},
	{"internal/hooks/hooks.go", "hooks not written by graft are in the way", "error text describing foreign hooks"},
	{"internal/iosched/iosched_linux.go", "graft with it (systemd-run -p OOMPolicy=continue keeps it running)", "prose describing the systemd scope"},
	{"internal/iosched/iosched_linux.go", "graft cannot place %s, and memory.max of it or a parent holds it", "error text describing a memory ceiling"},
	{"internal/iosched/iosched_linux.go", "graft cannot place %s, and neither it nor a parent has memory.max%s", "error text describing a missing memory ceiling"},
	{"internal/proc/refused.go", "run graft outside the sandbox", "error text describing sandbox privilege limits"},
	{"internal/services/services.go", "a process graft did not start", "error text describing process ownership"},
	{"internal/stats/reruns.go", "the graft that ran them ran them again", "report prose describing flaky task reruns"},
	{"internal/tasks/cache.go", "graft task cache 1", "cache hash version marker"},
	{"internal/tasks/snapshot.go", "a graft that ended stays", "error text describing a leftover clone"},
	{"internal/testdb/testdb.go", "graft does not know where its data lives", "error text describing a missing database volume"},
	{"internal/testdb/testdb.go", "let graft bring up its own test database", "error text describing database startup"},
	{"internal/tools/tools.go", "in the graft user config to install elsewhere", "warning text describing tool installation configuration"},
	{"testdb/testdb.go", "the graft task with test_db: true sets it", "prose explaining test database task setup"},
}

func pathWord(s string) bool { return pathWordPattern.MatchString(s) }

func constantString(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)
		return value, err == nil
	case *ast.ParenExpr:
		return constantString(expr.X)
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, ok := constantString(expr.X)
		if !ok {
			return "", false
		}
		right, ok := constantString(expr.Y)
		return left + right, ok
	default:
		return "", false
	}
}

func checkGoHintSynopsis(t *testing.T, entries []registryCommand, source, synopsis string) {
	t.Helper()
	for _, start := range graftWord.FindAllStringIndex(synopsis, -1) {
		rest := synopsis[start[1]:]
		if i := strings.IndexAny(rest, "\n;`"); i >= 0 {
			rest = rest[:i]
		}
		tokens := docToken.FindAllString(rest, -1)
		var command *registryCommand
		used := 0
		for _, entry := range entries {
			if len(entry.Path) > used && len(tokens) >= len(entry.Path) && slices.Equal(entry.Path, tokens[:len(entry.Path)]) {
				copyEntry := entry
				command, used = &copyEntry, len(entry.Path)
			}
		}
		if command == nil {
			if len(tokens) > 0 && pathWord(tokens[0]) {
				t.Errorf("%s: graft: unknown synopsis command %s", source, tokens[0])
			}
			continue
		}
		for _, token := range tokens[used:] {
			if token == "--" {
				continue
			}
			if strings.HasPrefix(token, "-") && token != "-" && !registryHasFlag(command, token) {
				t.Errorf("%s: graft %s: unknown synopsis flag %s", source, strings.Join(command.Path, " "), token)
			}
		}
	}
}

func registryHasFlag(command *registryCommand, name string) bool {
	if slices.Contains(command.Flags, name) {
		return true
	}
	if strings.HasPrefix(name, "--%") {
		return slices.ContainsFunc(command.Flags, func(flag string) bool { return strings.HasPrefix(flag, "--") })
	}
	if len(name) == 2 && name[0] == '-' {
		return slices.Contains(command.Flags, "--"+name[1:])
	}
	return false
}

func removeGoHintExceptions(path, value string) (string, []int, []string) {
	var found []int
	var problems []string
	for i, exception := range goHintExceptions {
		if filepath.ToSlash(path) != exception.file {
			continue
		}
		count, match := boundedHintException(value, exception.literal)
		if count == 0 {
			continue
		}
		if count != 1 {
			problems = append(problems, exception.file+" exception span occurs more than once: "+strconv.Quote(exception.literal))
		}
		found = append(found, i)
		value = value[:match] + value[match+len(exception.literal):]
	}
	return value, found, problems
}

// An exception ends at a word boundary so a suffix added to its last word
// cannot hide a command mention (for example, "graft itselfx").
func boundedHintException(value, fragment string) (int, int) {
	count, first := 0, -1
	for from := 0; from <= len(value)-len(fragment); {
		relative := strings.Index(value[from:], fragment)
		if relative < 0 {
			break
		}
		start := from + relative
		end := start + len(fragment)
		if end == len(value) || !hintWordContinuation(value[end:]) {
			if first < 0 {
				first = start
			}
			count++
		}
		from = start + 1
	}
	return count, first
}

func hintWordContinuation(s string) bool {
	if s == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

func editDistanceAtMostOne(a, b string) bool {
	if a == b {
		return true
	}
	if len(a)-len(b) > 1 || len(b)-len(a) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			i++
			j++
			continue
		}
		if len(a) == len(b) && i+1 < len(a) && j+1 < len(b) && a[i] == b[j+1] && a[i+1] == b[j] {
			edits++
			if edits > 1 {
				return false
			}
			i += 2
			j += 2
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(a) > len(b) {
			i++
		} else if len(b) > len(a) {
			j++
		} else {
			i++
			j++
		}
	}
	if i < len(a) || j < len(b) {
		edits++
	}
	return edits <= 1
}

func nearBuiltinCommand(word string, entries []registryCommand) bool {
	for _, entry := range entries {
		if editDistanceAtMostOne(word, entry.Path[0]) {
			return true
		}
	}
	return false
}

func configuredProjectTasks(t *testing.T) map[string]bool {
	t.Helper()
	cfg, err := config.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	tasks := make(map[string]bool, len(cfg.Tasks))
	for name := range cfg.Tasks {
		tasks[name] = true
	}
	return tasks
}

func TestRegistryProjectTaskCanResembleBuiltin(t *testing.T) {
	entries := registryForTest(t)
	tasks := configuredProjectTasks(t)
	tasks["comit"] = true // synthetic project task, one edit from built-in "commit"
	checkDocExampleMode(t, entries, "test", "run graft comit --", true, tasks)
}

func TestRegistryAdjacentTranspositionIsOneEdit(t *testing.T) {
	if !editDistanceAtMostOne("mrege", "merge") {
		t.Fatal("adjacent transposition in mrege should be one edit from merge")
	}
	if !nearBuiltinCommand("mrege", registryForTest(t)) {
		t.Fatal("mrege should be recognized as a typo of built-in merge")
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

func TestRegistryGoHintAndAnd(t *testing.T) {
	checkDocExample(t, registryForTest(t), "test", "graft init && graft amend --reset-author")
}

// TestRegistryGoHints checks commands shown to users by non-test Go code.
// Keep the extraction small and feed each literal through the documentation
// parser above so paths and flags use the same registry rules.
func TestRegistryGoHints(t *testing.T) {
	entries := registryForTest(t)
	projectTasks := configuredProjectTasks(t)
	paths := map[string]bool{}
	for _, e := range entries {
		paths[strings.Join(e.Path, " ")] = true
	}
	fset := token.NewFileSet()
	files := 0
	commands := map[string]bool{}
	type literalPosition struct {
		file   string
		offset int
	}
	checkedLiterals := map[literalPosition]bool{}
	exceptionCounts := make([]int, len(goHintExceptions))
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && (d.Name() == ".git" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		type sourceString struct {
			value string
			pos   token.Pos
			basic bool
		}
		var stringsInFile []sourceString
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					value, err := strconv.Unquote(n.Value)
					if err != nil {
						t.Errorf("%s: unquote string literal: %v", path, err)
					} else {
						stringsInFile = append(stringsInFile, sourceString{value: value, pos: n.Pos(), basic: true})
					}
				}
			case *ast.BinaryExpr:
				if value, ok := constantString(n); ok {
					stringsInFile = append(stringsInFile, sourceString{value: value, pos: n.Pos()})
				}
			}
			return true
		})
		for _, source := range stringsInFile {
			value, exceptions, exceptionProblems := removeGoHintExceptions(path, source.value)
			for _, problem := range exceptionProblems {
				t.Error(problem)
			}
			if source.basic {
				for _, exception := range exceptions {
					exceptionCounts[exception]++
				}
				if graftWord.MatchString(value) {
					checkedLiterals[literalPosition{file: filepath.ToSlash(path), offset: fset.Position(source.pos).Offset}] = true
				}
			}
			// docExamples handles inline and fenced command examples; checking
			// the whole literal also covers plain refusal and warning text.
			if strings.Contains(value, "[") && strings.HasPrefix(strings.TrimSpace(value), "graft ") {
				checkGoHintSynopsis(t, entries, path, value)
			} else {
				examples := docExamples(value)
				examples = append(examples, value)
				for _, example := range examples {
					checkDocExampleMode(t, entries, path, example, true, projectTasks)
				}
			}
			for _, loc := range graftWord.FindAllStringIndex(value, -1) {
				rest := value[loc[1]:]
				if strings.HasPrefix(rest, "%") {
					continue // a format substitution supplies this command word
				}
				word := docToken.FindString(rest)
				if word == "" {
					continue
				}
				// The shared parser checks both registered paths and path-shaped
				// misspellings, regardless of the prose before “graft”.
				if !paths[word] && pathWord(word) {
					if len(docToken.FindAllString(rest, -1)) == 1 || strings.HasPrefix(strings.TrimSpace(rest), "-") {
						t.Errorf("%s: graft: unknown command %s", path, word)
					}
				}
				if paths[word] || !paths[word] && pathWord(word) {
					commands[path+":"+strconv.Itoa(fset.Position(source.pos).Offset)+":"+strconv.Itoa(loc[0])] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no non-test Go files found")
	}
	for i, count := range exceptionCounts {
		if count != 1 {
			exception := goHintExceptions[i]
			t.Errorf("stale Go hint exception for %s (%q): matched %d literals; reason: %s", exception.file, exception.literal, count, exception.reason)
		}
	}
	t.Logf("checked %d command mentions in %d string literals across %d non-test Go files", len(commands), len(checkedLiterals), files)
}
