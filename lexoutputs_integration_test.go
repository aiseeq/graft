package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLexOutputsGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell lex stub")
	}
	for _, tc := range []struct {
		name       string
		files      map[string]string
		config     string
		remove     []string
		exit       string
		missing    bool
		wantCode   int
		wantOutput []string
		codices    []string
	}{
		{name: "no header skips step", files: map[string]string{"notes.md": "ordinary notes\n"}},
		{name: "checks unique codex once", files: map[string]string{
			"a.md": "<!-- lex: {{same}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n",
			"b.md": "<!-- lex: {{same}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n",
		}, codices: []string{"same"}},
		{name: "checks every codex after refusal", files: map[string]string{
			"a.md": "<!-- lex: {{codex-a}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n",
			"b.md": "<!-- lex: {{codex-b}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n",
		}, exit: "1", wantCode: 1, wantOutput: []string{"lex absorb", "lex binary", "codex-a", "codex-b"}, codices: []string{"codex-a", "codex-b"}},
		{name: "lex failure output", files: map[string]string{"projectA-CLAUDE.md": "<!-- lex: {{codex}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n"}, exit: "1", wantCode: 1, wantOutput: []string{"projectA-CLAUDE.md: edited by hand", "lex absorb"}, codices: []string{"codex"}},
		{name: "unexpected lex exit has no absorb hint", files: map[string]string{"projectA-CLAUDE.md": "<!-- lex: {{codex}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n"}, exit: "2", wantCode: 1, wantOutput: []string{"projectA-CLAUDE.md: edited by hand"}, codices: []string{"codex"}},
		{name: "missing lex", files: map[string]string{"projectA-CLAUDE.md": "<!-- lex: {{codex}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n"}, missing: true, wantCode: 1, wantOutput: []string{"lex header found", "lex was not found in PATH"}},
		{name: "malformed header names file", files: map[string]string{"projectA-CLAUDE.md": "<!-- lex: generated output -->\nbody\n"}, wantCode: 1, wantOutput: []string{"projectA-CLAUDE.md", "manifest"}},
		{name: "disabled Go checks still run lex", files: map[string]string{"projectA-CLAUDE.md": "<!-- lex: {{codex}}/manifest/projectA-CLAUDE.md.yaml -->\nbody\n"}, config: "schema: 1\nversion: {mode: none}\ngo_vet: {enabled: false, reason: synthetic project}\ngo_fix: {enabled: false}\ngo_fmt: {enabled: false, reason: synthetic project}\n", codices: []string{"codex"}},
		{name: "deleted tracked Markdown is skipped", files: map[string]string{"old.md": "<!-- lex: {{codex}}/manifest/projectA-CLAUDE.md.yaml -->\n"}, remove: []string{"old.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.files == nil {
				tc.files = map[string]string{}
			}
			if tc.config == "" {
				tc.config = "schema: 1\nversion: {mode: none}\n"
			}
			codexRoot := t.TempDir()
			codexPaths := make(map[string]string)
			for _, name := range []string{"same", "codex", "codex-a", "codex-b"} {
				codexDir := filepath.Join(codexRoot, name)
				manifestDir := filepath.Join(codexDir, "manifest")
				if err := os.MkdirAll(manifestDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(manifestDir, "projectA-CLAUDE.md.yaml"), []byte("schema: 1\n"), 0644); err != nil {
					t.Fatal(err)
				}
				codexPaths[name] = filepath.ToSlash(codexDir)
			}
			for name, content := range tc.files {
				for codex, path := range codexPaths {
					content = strings.ReplaceAll(content, "{{"+codex+"}}", path)
				}
				tc.files[name] = content
			}
			tc.files[".graft.yaml"] = tc.config
			f := newRepo(t, tc.files)
			for _, name := range tc.remove {
				f.git("add", name)
				if err := os.Remove(filepath.Join(f.work, name)); err != nil {
					t.Fatal(err)
				}
			}
			stubDir := t.TempDir()
			argsFile := filepath.Join(t.TempDir(), "args")
			stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$LEX_ARGS_FILE\"\n" + "echo 'projectA-CLAUDE.md: edited by hand'\nexit \"${LEX_EXIT_CODE:-0}\"\n"
			if err := os.WriteFile(filepath.Join(stubDir, "lex"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			path := stubDir + string(os.PathListSeparator) + os.Getenv("PATH")
			if tc.missing {
				path = t.TempDir()
				gitPath, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(gitPath, filepath.Join(path, "git")); err != nil {
					t.Fatal(err)
				}
			}
			f.env = []string{"PATH=" + path, "LEX_ARGS_FILE=" + argsFile, "LEX_EXIT_CODE=" + tc.exit}
			out, code := f.graft("", "gate")
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.wantCode, out)
			}
			if tc.name == "no header skips step" && strings.Contains(out, "lex check --codex") {
				t.Errorf("unexpected lex step:\n%s", out)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if tc.exit != "" && !strings.Contains(out, filepath.Join(stubDir, "lex")) {
				t.Errorf("failure does not identify the lex binary that ran:\n%s", out)
			}
			if tc.name == "unexpected lex exit has no absorb hint" && strings.Contains(out, "lex absorb") {
				t.Errorf("non-refusal exit must not suggest lex absorb:\n%s", out)
			}
			if tc.name == "missing lex" && strings.Contains(out, "lex absorb") {
				t.Errorf("missing executable must not suggest lex absorb:\n%s", out)
			}
			gotArgs, err := os.ReadFile(argsFile)
			if tc.missing || tc.name == "no header skips step" || tc.name == "malformed header names file" || tc.name == "deleted tracked Markdown is skipped" {
				if !os.IsNotExist(err) {
					t.Errorf("lex unexpectedly called, args %q (err %v)", gotArgs, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var wantArgs strings.Builder
			for _, codex := range tc.codices {
				wantArgs.WriteString("check\n--codex\n" + codexPaths[codex] + "\n--outputs-under\n" + f.work + "\n")
			}
			if string(gotArgs) != wantArgs.String() {
				t.Errorf("lex args:\n%s\nwant:\n%s", gotArgs, wantArgs.String())
			}
		})
	}
}

func TestLexOutputsDoesNotListWorktreesWithoutHeaders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell git wrapper")
	}
	f := newRepo(t, map[string]string{
		".graft.yaml": "schema: 1\nversion: {mode: none}\n",
		"notes.md":    "ordinary notes\n",
	})
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	callFile := filepath.Join(t.TempDir(), "git-calls")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_CALLS_FILE\"\nexec \"$REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0755); err != nil {
		t.Fatal(err)
	}
	f.env = []string{
		"PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CALLS_FILE=" + callFile,
		"REAL_GIT=" + realGit,
	}
	out, code := f.graft("", "gate")
	if code != 0 {
		t.Fatalf("gate: exit %d\n%s", code, out)
	}
	calls, err := os.ReadFile(callFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "worktree list --porcelain") {
		t.Fatalf("gate listed worktrees without a lex header:\n%s", calls)
	}
}

func TestLexOutputsSkipsMarkdownSymlinkToDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creates a directory symlink")
	}
	f := newRepo(t, map[string]string{".graft.yaml": "schema: 1\nversion: {mode: none}\n"})
	target := filepath.Join(f.work, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.work, "directory.md")); err != nil {
		t.Fatal(err)
	}
	out, code := f.graft("", "gate")
	if code != 0 || strings.Contains(out, "lex check --codex") {
		t.Fatalf("directory symlink should be skipped: exit %d\n%s", code, out)
	}
}

func TestLexOutputsUsesMainCheckoutForWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell lex stub and git worktree")
	}
	codexDir := filepath.ToSlash(filepath.Join(t.TempDir(), "synthetic-codex"))
	manifestDir := filepath.Join(filepath.FromSlash(codexDir), "manifest")
	if err := os.MkdirAll(manifestDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "projectA-CLAUDE.md.yaml"), []byte("schema: 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f := newRepo(t, map[string]string{
		".graft.yaml":        "schema: 1\nversion: {mode: none}\n",
		"projectA-CLAUDE.md": "<!-- lex: " + codexDir + "/manifest/projectA-CLAUDE.md.yaml -->\nsynthetic instructions\n",
	})
	f.run(f.work, "git", "add", "-A")
	f.run(f.work, "git", "commit", "-m", "base")
	worktree := filepath.Join(f.base, "worktree")
	f.run(f.work, "git", "worktree", "add", "-b", "feature/PROJ-42-lex", worktree)
	stubDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$LEX_ARGS_FILE\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "lex"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	f.env = []string{"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"), "LEX_ARGS_FILE=" + argsFile}

	mainOut, mainCode := f.graft("", "gate")
	if mainCode != 0 {
		t.Fatalf("main checkout gate: exit %d\n%s", mainCode, mainOut)
	}
	mainArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantMain := "check\n--codex\n" + codexDir + "\n--outputs-under\n" + f.work + "\n"
	if string(mainArgs) != wantMain {
		t.Fatalf("main checkout lex args:\n%s\nwant:\n%s", mainArgs, wantMain)
	}
	if err := os.WriteFile(argsFile, nil, 0644); err != nil {
		t.Fatal(err)
	}
	workOut, workCode := f.exec(worktree, "", graftBin, "gate")
	if workCode != 0 {
		t.Fatalf("worktree gate: exit %d\n%s", workCode, workOut)
	}
	workArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantWorktree := wantMain + "--tree\n" + worktree + "\n"
	if string(workArgs) != wantWorktree {
		t.Fatalf("worktree lex args:\n%s\nwant:\n%s", workArgs, wantWorktree)
	}
}

func TestLexOutputsResolvesMainCheckoutLayouts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell lex stub and git repository layouts")
	}
	for _, layout := range []string{"submodule", "separate git dir", "separate git dir worktree", "separate git dir worktree without config", "submodule worktree"} {
		t.Run(layout, func(t *testing.T) {
			f := newRepo(t, map[string]string{})
			codexDir := filepath.ToSlash(filepath.Join(t.TempDir(), "synthetic-codex"))
			manifestDir := filepath.Join(filepath.FromSlash(codexDir), "manifest")
			if err := os.MkdirAll(manifestDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(manifestDir, "projectA-CLAUDE.md.yaml"), []byte("schema: 1\n"), 0644); err != nil {
				t.Fatal(err)
			}
			argsFile := filepath.Join(t.TempDir(), "lex-args")
			stubDir := t.TempDir()
			stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$LEX_ARGS_FILE\"\n"
			if err := os.WriteFile(filepath.Join(stubDir, "lex"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			f.env = []string{"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"), "LEX_ARGS_FILE=" + argsFile}
			writeLexRepo := func(dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					".graft.yaml":        "schema: 1\nversion: {mode: none}\n",
					"projectA-CLAUDE.md": "<!-- lex: " + codexDir + "/manifest/projectA-CLAUDE.md.yaml -->\nsynthetic instructions\n",
				} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			initAndCommit := func(dir string, initArgs ...string) {
				t.Helper()
				args := append([]string{"init", "-q"}, initArgs...)
				f.run(dir, "git", args...)
				f.run(dir, "git", "config", "user.name", "Synthetic User")
				f.run(dir, "git", "config", "user.email", "synthetic@example.invalid")
				f.run(dir, "git", "add", "-A")
				f.run(dir, "git", "commit", "-m", "initial")
			}
			checkArgs := func(dir, want string) {
				t.Helper()
				out, code := f.exec(dir, "", graftBin, "gate")
				if code != 0 {
					t.Fatalf("gate from %s: exit %d\n%s", dir, code, out)
				}
				got, err := os.ReadFile(argsFile)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Fatalf("lex args:\n%s\nwant:\n%s", got, want)
				}
			}
			checkFailure := func(dir, wantReason string) {
				t.Helper()
				out, code := f.exec(dir, "", graftBin, "gate")
				if code == 0 || !strings.Contains(out, wantReason) {
					t.Fatalf("gate from %s: exit %d, want failure containing %q\n%s", dir, code, wantReason, out)
				}
				if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
					t.Fatalf("lex was called despite checkout resolution failure (stat err %v)", err)
				}
			}
			baseArgs := "check\n--codex\n" + codexDir + "\n--outputs-under\n"
			switch layout {
			case "submodule":
				source := filepath.Join(f.base, "source")
				writeLexRepo(source)
				initAndCommit(source)
				writeLexRepo(f.work)
				initAndCommit(f.work)
				f.run(f.work, "git", "-c", "protocol.file.allow=always", "submodule", "add", source, "nested")
				f.run(f.work, "git", "add", "-A")
				f.run(f.work, "git", "commit", "-m", "add synthetic submodule")
				submodule := filepath.Join(f.work, "nested")
				checkArgs(submodule, baseArgs+submodule+"\n")
			case "separate git dir":
				tree := filepath.Join(f.base, "separate-tree")
				gitDir := filepath.Join(f.base, "git-metadata")
				writeLexRepo(tree)
				initAndCommit(tree, "--separate-git-dir", gitDir, tree)
				checkArgs(tree, baseArgs+tree+"\n")
			case "separate git dir worktree":
				tree := filepath.Join(f.base, "separate-tree")
				gitDir := filepath.Join(f.base, "git-metadata")
				writeLexRepo(tree)
				initAndCommit(tree, "--separate-git-dir", gitDir, tree)
				f.run(tree, "git", "config", "core.worktree", tree)
				worktree := filepath.Join(f.base, "linked-tree")
				f.run(tree, "git", "worktree", "add", "-b", "feature/PROJ-42-lex", worktree)
				checkArgs(worktree, baseArgs+tree+"\n--tree\n"+worktree+"\n")
			case "separate git dir worktree without config":
				tree := filepath.Join(f.base, "separate-tree")
				gitDir := filepath.Join(f.base, "git-metadata")
				writeLexRepo(tree)
				initAndCommit(tree, "--separate-git-dir", gitDir, tree)
				worktree := filepath.Join(f.base, "linked-tree")
				f.run(tree, "git", "worktree", "add", "-b", "feature/PROJ-42-lex", worktree)
				checkFailure(worktree, "main checkout is not configured")
			case "submodule worktree":
				source := filepath.Join(f.base, "source")
				writeLexRepo(source)
				initAndCommit(source)
				writeLexRepo(f.work)
				initAndCommit(f.work)
				f.run(f.work, "git", "-c", "protocol.file.allow=always", "submodule", "add", source, "nested")
				f.run(f.work, "git", "add", "-A")
				f.run(f.work, "git", "commit", "-m", "add synthetic submodule")
				submodule := filepath.Join(f.work, "nested")
				worktree := filepath.Join(f.base, "linked-submodule")
				f.run(submodule, "git", "worktree", "add", "-b", "feature/PROJ-42-submodule", worktree)
				checkArgs(worktree, baseArgs+submodule+"\n--tree\n"+worktree+"\n")
			}
		})
	}
}
