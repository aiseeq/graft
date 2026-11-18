package app

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestCheckTakeBackRefusesWithBoundedCommitList(t *testing.T) {
	repo := newEmptyWithheldRepo(t)
	first := repo.commitText("work.txt", "0", "base")
	for i := 1; i <= 22; i++ {
		repo.commitText("work.txt", string(rune('a'+i)), "take-back commit")
	}
	var out bytes.Buffer
	a := &App{Stdout: &out}
	r := &deployRun{name: "uat", repo: repo.repo, head: first, prev: repo.git("rev-parse", "HEAD"), target: &config.DeployTarget{}}
	err := a.checkTakeBack(r, false)
	if err == nil || !strings.Contains(err.Error(), "takes back 22 deployed commits") || !strings.Contains(err.Error(), "… and 2 more") {
		t.Fatalf("checkTakeBack error = %v", err)
	}
	if strings.Count(err.Error(), "take-back commit") != 20 {
		t.Errorf("listed commit lines = %d, want 20", strings.Count(err.Error(), "take-back commit"))
	}
}

func TestCheckTakeBackReasonAndNoTakeBack(t *testing.T) {
	repo := newEmptyWithheldRepo(t)
	head := repo.commitText("work.txt", "head", "head")
	prev := repo.commitText("work.txt", "deployed", "deployed")
	var out bytes.Buffer
	a := &App{Stdout: &out}
	r := &deployRun{name: "uat", repo: repo.repo, head: head, prev: prev, target: &config.DeployTarget{}, takeBackReason: "intentional rollback"}
	if err := a.checkTakeBack(r, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "intentional rollback") {
		t.Errorf("take-back reason not printed: %q", out.String())
	}
	out.Reset()
	r.prev = head
	if err := a.checkTakeBack(r, false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("no-take-back output = %q", out.String())
	}
}
