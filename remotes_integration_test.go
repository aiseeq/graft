package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDeployRefusesWorkOnAnotherRemote: a deploy fetches every remote and
// refuses while another remote's branch has commits whose content the deploy
// remote's lacks, a pull request merged on the mirror say; the same change
// cherry-picked under another hash is there by content and passes.
func TestDeployRefusesWorkOnAnotherRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy scripts use sh")
	}
	out := filepath.Join(t.TempDir(), "runs")
	f := newRepo(t, map[string]string{
		".graft.yaml": `schema: 1
version: {mode: file}
envs:
  local: {}
deploy:
  remote: origin
  targets:
    prod: {env: local, run: [sh, deploy.sh]}
`,
		"VERSION":   "1.0.0\n",
		"deploy.sh": "echo deployed >> " + out + "\n",
	}, "origin", "mirror")
	f.git("checkout", "-q", "-b", "main")
	f.write("VERSION", "1.0.1\n")
	f.mustPlainCommit("feat: base")
	f.git("push", "-q", "origin", "main")
	f.git("push", "-q", "mirror", "main")

	// A pull request merged on the mirror: a merge commit and its commit.
	clone := filepath.Join(f.base, "clone")
	f.run(f.base, "git", "clone", "-q", "-b", "main", f.remote("mirror"), clone)
	f.gitIn(clone, "checkout", "-q", "-b", "pr")
	writeFile(t, filepath.Join(clone, "pr.txt"), "pr\n")
	f.gitIn(clone, "add", "pr.txt")
	f.gitIn(clone, "-c", "user.name=op", "-c", "user.email=op@example.com", "commit", "-q", "-m", "feat: from the pull request")
	pr := f.gitIn(clone, "rev-parse", "HEAD")
	f.gitIn(clone, "checkout", "-q", "main")
	f.gitIn(clone, "-c", "user.name=op", "-c", "user.email=op@example.com", "merge", "-q", "--no-ff", "-m", "Merge pull request #1", "pr")
	f.gitIn(clone, "push", "-q", "origin", "main")

	got, code := f.graft("", "deploy", "prod")
	if code == 0 || !strings.Contains(got, "mirror/main has 1 commit that origin/main lacks: "+pr[:12]+" feat: from the pull request") ||
		!strings.Contains(got, "git merge mirror/main") {
		t.Errorf("a pull request merged on the mirror: exit %d\n%s", code, got)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the deploy script ran: %v", err)
	}

	// The same change under another hash is there by content.
	f.git("fetch", "-q", "mirror")
	f.git("cherry-pick", pr)
	f.git("push", "-q", "origin", "main")
	f.mustGraft("deploy", "prod")

	f.git("remote", "set-url", "mirror", filepath.Join(f.base, "gone.git"))
	if got, code := f.graft("", "deploy", "--redeploy", "prod"); code == 0 || !strings.Contains(got, "git fetch mirror failed") {
		t.Errorf("an unreachable remote: exit %d\n%s", code, got)
	}
}
