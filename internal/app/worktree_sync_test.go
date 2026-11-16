package app

import "testing"

func TestWorktreeSyncGoModHintRequiresOnlyRequireVersionChange(t *testing.T) {
	ours := "module example.com/projectA\n\ngo 1.25\n\nrequire example.com/akit v1.9.0\n\nreplace example.com/dep => ../worktree\n"
	theirs := "module example.com/projectA\n\ngo 1.27\n\nrequire example.com/akit v1.10.0\n\nreplace example.com/dep => ../trunk\n"
	hint, err := worktreeSyncGoModVersionHint(ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if hint != "" {
		t.Fatalf("other go.mod directives changed, but got a version hint: %s", hint)
	}
}
