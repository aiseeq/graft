package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/app"
	"github.com/aiseeq/graft/v2/internal/config"
)

func registryForTest(t *testing.T) []registryCommand {
	t.Helper()
	entries, err := registry(&app.App{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func entryFor(entries []registryCommand, path string) *registryCommand {
	for i := range entries {
		if strings.Join(entries[i].Path, " ") == path {
			return &entries[i]
		}
	}
	return nil
}

func TestRegistryJSON(t *testing.T) {
	var out bytes.Buffer
	a := &app.App{Dir: t.TempDir(), Stdout: &out}
	if err := helpCmd(a, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var doc registryDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Tool != "graft" || doc.Version != toolVersion() || len(doc.Commands) < 20 || len(doc.Tasks) != 0 || doc.TasksError == "" {
		t.Fatalf("outside project: %+v", doc)
	}
}

// The registry records flag sets before Parse; check every source flag reaches
// at least one path, including flags on a shared subcommand parser.
func TestRegistryFlagSets(t *testing.T) {
	entries := registryForTest(t)
	seen := map[string]bool{}
	for _, spec := range registrySpecs {
		if spec.source == "" || seen[spec.source] {
			continue
		}
		seen[spec.source] = true
		fs, err := recordedFlags(&app.App{}, spec.source)
		if err != nil {
			t.Fatal(err)
		}
		fs.VisitAll(func(f *flag.Flag) {
			found := false
			for _, candidate := range registrySpecs {
				if candidate.source == spec.source && slices.Contains(entryFor(entries, candidate.path).Flags, "--"+f.Name) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: flag --%s missing from registry", spec.source, f.Name)
			}
		})
	}
	for name := range subcommands {
		if entryFor(entries, name) == nil {
			t.Errorf("dispatch command %s missing from registry", name)
		}
	}
	for _, name := range config.DeploySubcommands {
		if entryFor(entries, "deploy "+name) == nil {
			t.Errorf("deploy %s missing from registry", name)
		}
	}
}

// Exec has a handwritten switch. Its case labels are the accepted spellings;
// comparing the syntax tree catches additions as well as stale entries.
func TestRegistryExecParser(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "parseExecFlag" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range c.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					actual = append(actual, name)
				}
			}
			return true
		})
	}
	slices.Sort(actual)
	want := slices.Clone(entryFor(registryForTest(t), "exec").Flags)
	slices.Sort(want)
	if !slices.Equal(actual, want) {
		t.Errorf("exec parser flags %v, registry %v", actual, want)
	}
}

var syntaxFlag = regexp.MustCompile(`(?:^|[ \t[(|])(--?[A-Za-z][A-Za-z0-9-]*)`)

func syntaxFlags(s string) []string {
	var flags []string
	for _, match := range syntaxFlag.FindAllStringSubmatch(s, -1) {
		flags = append(flags, match[1])
	}
	return flags
}

func TestRegistryHelpSyntax(t *testing.T) {
	entries := registryForTest(t)
	global := []string{"-C", "--help", "-h", "-v"}
	listed := map[string][]string{}
	for _, section := range commands {
		for _, line := range section.syntax(nil) {
			tokens := docToken.FindAllString(strings.TrimPrefix(line, "graft "), -1)
			var matched *registryCommand
			for i := range entries {
				e := &entries[i]
				if len(e.Path) <= len(tokens) && (matched == nil || len(e.Path) > len(matched.Path)) && slices.Equal(e.Path, tokens[:len(e.Path)]) {
					matched = e
				}
			}
			if matched == nil {
				continue // generic task and global help forms
			}
			path := strings.Join(matched.Path, " ")
			for _, name := range syntaxFlags(line) {
				if slices.Contains(global, name) {
					continue
				}
				if !slices.Contains(matched.Flags, name) {
					t.Errorf("help syntax %s: unknown flag %s", path, name)
				}
				listed[path] = append(listed[path], name)
				if path == "flags status" {
					listed["flags"] = append(listed["flags"], name)
				}
			}
		}
	}
	for _, e := range entries {
		path := strings.Join(e.Path, " ")
		for _, name := range e.Flags {
			if !strings.HasPrefix(name, "--") || slices.Contains(global, name) {
				continue
			}
			if !slices.Contains(listed[path], name) && !slices.Contains(listed[path], "-"+strings.TrimPrefix(name, "--")) {
				t.Errorf("%s: flag %s missing from help syntax", path, name)
			}
		}
	}
}

func TestRegistryExecAccepted(t *testing.T) {
	for _, name := range entryFor(registryForTest(t), "exec").Flags {
		args := []string{name}
		switch name {
		case "--env":
			args = append(args, "NAME")
		case "--settle":
			args = append(args, "1s")
		case "--cores":
			args = append(args, "1")
		case "--memory-max":
			args = append(args, "1G")
		}
		var opts app.ExecOptions
		var local bool
		var pass []string
		if _, err := parseExecFlag(args, &opts, &local, &pass); err != nil {
			t.Errorf("exec %s: %v", name, err)
		}
	}
}

func TestRegistryProjectTasks(t *testing.T) {
	f := tasksRepo(t, "tasks:\n  test:\n    desc: test\n    run: [true]\n  lint:\n    run: [true]\n", nil)
	out, code := f.exec(f.base, "", graftBin, "-C", f.work, "help", "--json")
	if code != 0 {
		t.Fatalf("help --json: exit %d: %s", code, out)
	}
	var doc registryDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.TasksError != "" || !slices.Equal(doc.Tasks, []string{"lint", "test"}) {
		t.Fatalf("project tasks: %v, error %q", doc.Tasks, doc.TasksError)
	}
	f.write(".graft.yaml", "schema: [invalid\n")
	out, code = f.graft("", "help", "--json")
	if code != 0 {
		t.Fatalf("invalid config: exit %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.TasksError == "" || len(doc.Tasks) != 0 {
		t.Fatalf("invalid config: %s (%v)", out, err)
	}
}

func TestRegistryManualParsers(t *testing.T) {
	entries := registryForTest(t)
	checks := []struct {
		file, function, path string
	}{
		{"main.go", "dispatch", "init"},
		{"main.go", "runTask", "run"},
		{"help.go", "helpCmd", "help"},
	}
	flagLiteral := regexp.MustCompile(`^--[a-z][a-z0-9-]*$`)
	for _, check := range checks {
		file, err := parser.ParseFile(token.NewFileSet(), check.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		actual := []string{}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != check.function {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil && flagLiteral.MatchString(value) && !slices.Contains(actual, value) {
					actual = append(actual, value)
				}
				return true
			})
		}
		want := slices.Clone(entryFor(entries, check.path).Flags)
		slices.Sort(actual)
		slices.Sort(want)
		if !slices.Equal(actual, want) {
			t.Errorf("%s parser flags %v, registry %v", check.path, actual, want)
		}
	}
	for action := range strings.SplitSeq(strings.TrimPrefix(app.TestDBUsage(), "graft testdb "), "|") {
		if entryFor(entries, "testdb "+action) == nil {
			t.Errorf("testdb %s missing from registry", action)
		}
	}
}

func TestRegistryDispatchPaths(t *testing.T) {
	entries := registryForTest(t)
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !slices.Contains([]string{"dispatch", "projectCmd", "worktreeCmd"}, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				path, err := strconv.Unquote(literal.Value)
				if err != nil || strings.HasPrefix(path, "-") {
					continue
				}
				if fn.Name.Name == "worktreeCmd" {
					path = "worktree " + path
				}
				if entryFor(entries, path) == nil {
					t.Errorf("%s dispatches %q, absent from registry", fn.Name.Name, path)
				}
			}
			return true
		})
	}
}

// TestRegistryGroupRest checks that a command with subcommands says "none"
// for its rest exactly when an unknown word after it is a usage error, so
// a misspelt subcommand in the docs is not taken for an argument.
func TestRegistryGroupRest(t *testing.T) {
	entries := registryForTest(t)
	for _, e := range entries {
		hasChild := slices.ContainsFunc(entries, func(c registryCommand) bool {
			return len(c.Path) > len(e.Path) && slices.Equal(c.Path[:len(e.Path)], e.Path)
		})
		if !hasChild {
			continue
		}
		var out bytes.Buffer
		a := &app.App{Stdout: &out, Stderr: &out}
		err := dispatch(t.Context(), a, append(slices.Clone(e.Path), "no-such-subcommand"))
		usage := errors.Is(err, errUsage)
		if usage != (e.Rest == "none") {
			t.Errorf("graft %s no-such-subcommand: usage error %v (%v), registry rest %q", strings.Join(e.Path, " "), usage, err, e.Rest)
		}
	}
}
