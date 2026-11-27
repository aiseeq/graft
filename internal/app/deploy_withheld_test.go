package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

type withheldRepo struct {
	t    *testing.T
	dir  string
	repo *gitx.Repo
}

func setJiraTestEnv(t *testing.T, baseURL string) {
	t.Helper()
	configRoot := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "jira.env")
	if err := os.WriteFile(envFile, []byte("JIRA_BASE_URL="+baseURL+"\nJIRA_EMAIL=synthetic@example.invalid\nJIRA_API_TOKEN="+"synthetic"+"-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configRoot, "graft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "graft", "config.yaml"), []byte("jira:\n  env_file: "+envFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)
}

func newWithheldRepo(t *testing.T, initial string) *withheldRepo {
	t.Helper()
	r := newEmptyWithheldRepo(t)
	r.commitText("work.txt", initial, "base")
	return r
}

func newEmptyWithheldRepo(t *testing.T) *withheldRepo {
	t.Helper()
	dir := t.TempDir()
	r := &withheldRepo{t: t, dir: dir}
	r.git("init", "-q")
	r.git("config", "user.name", "Test")
	r.git("config", "user.email", "test@example.invalid")
	r.git("config", "commit.gpgsign", "false")
	var err error
	r.repo, err = gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *withheldRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *withheldRepo) writePath(name string, content *string) {
	r.t.Helper()
	file := filepath.Join(r.dir, name)
	if content == nil {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			r.t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(*content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *withheldRepo) commitPath(name string, content *string, subject string) string {
	r.t.Helper()
	r.writePath(name, content)
	r.git("add", "-A")
	r.git("commit", "-qm", subject)
	return r.head()
}

func (r *withheldRepo) commitText(name, content, subject string) string {
	r.t.Helper()
	return r.commitPath(name, &content, subject)
}

func (r *withheldRepo) commitAll(subject string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-qm", subject)
	return r.head()
}

func (r *withheldRepo) revert(sha string) {
	r.t.Helper()
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "revert", "--no-edit", sha)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git revert %s: %v\n%s", sha, err, out)
	}
}

func (r *withheldRepo) head() string {
	r.t.Helper()
	return r.git("rev-parse", "HEAD")
}

func partitionForTest(t *testing.T, r *withheldRepo, prev, head, w string, versionFiles ...string) ([]string, []string) {
	t.Helper()
	paths, err := repoPaths(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, versionFiles, paths)
	if err != nil {
		t.Fatal(err)
	}
	return active, held
}

func TestPartitionWithheldUsesChangedFiles(t *testing.T) {
	t.Run("hotfix does not touch overlaid withheld work", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("work.txt", "original\n", "feat: PROJ-123 work")
		r.commitText("work.txt", "base\n", "fix: PROJ-123 cancel work")
		r.commitText("work.txt", "reworked\n", "feat: PROJ-123 followup")
		prev := r.head()
		head := r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 hotfix")
		active, held := partitionForTest(t, r, prev, head, w)
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want W retained and unchecked", active, held)
		}
	})

	t.Run("hotfix does not recheck work already shipped unapproved", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("work.txt", "original\n", "feat: PROJ-123 work")
		prev := w
		head := r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 hotfix")
		active, held := partitionForTest(t, r, prev, head, w)
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want W retained and unchecked", active, held)
		}
	})

	for _, tc := range []struct {
		name    string
		prepare func(*withheldRepo) (w, prev, head string)
	}{
		{
			name: "partial restoration",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitText("work.txt", "new1\nmiddle\nnew2\n", "feat: PROJ-123 work")
				r.revert(w)
				prev = r.head()
				head = r.commitText("work.txt", "new1\nmiddle\nold2\n", "fix: PROJ-123 partial restore")
				return
			},
		},
		{
			name: "independent replacement",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitText("work.txt", "secret-feature\n", "feat: PROJ-123 work")
				r.revert(w)
				prev = r.commitText("work.txt", "unrelated\n", "fix: independent replacement")
				head = r.commitText("work.txt", "secret-feature\n", "fix: replace again")
				return
			},
		},
		{
			name: "file deletion",
			prepare: func(r *withheldRepo) (w, prev, head string) {
				w = r.commitPath("work.txt", nil, "feat: PROJ-123 delete work file")
				r.revert(w)
				prev = r.commitText("work.txt", "other\n", "fix: restore independently")
				head = r.commitPath("work.txt", nil, "fix: delete file")
				return
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWithheldRepo(t, "old1\nmiddle\nold2\n")
			w, prev, head := tc.prepare(r)
			active, held := partitionForTest(t, r, prev, head, w)
			if len(active) != 1 || active[0] != w || len(held) != 0 {
				t.Fatalf("active %v, held %v; want W checked", active, held)
			}
		})
	}

	t.Run("version-only commit uses its real change", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		w := r.commitText("VERSION", "1\n", "feat: PROJ-123 version change")
		prev := r.head()
		r.revert(w)
		head := r.head()
		active, held := partitionForTest(t, r, prev, head, w, "./VERSION")
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want reverted version change retained", active, held)
		}
	})

	t.Run("root withheld commit paths", func(t *testing.T) {
		r := newEmptyWithheldRepo(t)
		w := r.commitText("work.txt", "root change\n", "feat: PROJ-123 root work")
		paths, err := withheldPaths(r.repo, w)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != "work.txt" {
			t.Fatalf("withheld paths %v; want [work.txt]", paths)
		}
		active, held, err := partitionWithheld(r.repo, w, w, []string{w}, nil, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 0 || len(held) != 1 || held[0] != w {
			t.Fatalf("active %v, held %v; want root W retained without error", active, held)
		}
	})

	t.Run("merge withheld commit paths use first parent", func(t *testing.T) {
		r := newWithheldRepo(t, "base\n")
		mainBranch := r.git("branch", "--show-current")
		r.git("checkout", "-qb", "feature")
		r.commitText("work.txt", "merged work\n", "feat: PROJ-123 merge side")
		r.git("checkout", "-q", mainBranch)
		r.commitText("hotfix.txt", "hotfix\n", "fix: PROJ-123 mainline")
		r.git("merge", "--no-ff", "-qm", "merge feature", "feature")
		w := r.head()
		paths, err := withheldPaths(r.repo, w)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || paths[0] != "work.txt" {
			t.Fatalf("withheld paths %v; want [work.txt]", paths)
		}
	})

}

func TestReview3OtherBranchRenamedRestore(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	mainBranch := r.git("branch", "--show-current")
	r.git("checkout", "-qb", "withheld")
	w := r.commitText("work.txt", "secret\n", "feat: PROJ-123 withheld work")
	r.git("checkout", "-q", mainBranch)
	r.git("mv", "work.txt", "renamed.txt")
	prev := r.commitAll("refactor: rename work file")
	head := r.commitText("renamed.txt", "secret\n", "feat: PROJ-456 restore work")
	active, held := partitionForTest(t, r, prev, head, w)
	if len(active) != 1 || active[0] != w || len(held) != 0 {
		t.Fatalf("active %v, held %v; want non-ancestor W checked", active, held)
	}
}

func TestReview3OtherBranchRenamedRestoreMixedVersionFileCode(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	r.writePath("VERSION", new("1.0.0\n"))
	r.writePath("version.go", new("// version: 1.0.0\nconst policy = \"old\"\n"))
	r.commitAll("chore: add version files")
	r.writePath("work.txt", new("secret\n"))
	r.writePath("version.go", new("// version: 1.0.1\nconst policy = \"secret\"\n"))
	w := r.commitAll("feat: PROJ-123 mixed work")
	prev := w
	head := r.commitText("version.go", "// version: 1.0.1\nconst policy = \"restored\"\n", "feat: PROJ-456 restore policy")
	versionCfg := config.Version{
		Mode: config.ModeFile,
		File: "VERSION",
		Sync: []config.SyncFile{{Path: "version.go", Format: config.FormatRegex, Pattern: `version: ([0-9]+\.[0-9]+\.[0-9]+)`, Regexp: regexp.MustCompile(`version: ([0-9]+\.[0-9]+\.[0-9]+)`)}},
	}
	paths := pathsForVersionDeploy(t, r, prev, head, versionCfg)
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, []string{"VERSION", "version.go"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != w || len(held) != 0 {
		t.Fatalf("active %v, held %v; want W checked for code changed in version.go", active, held)
	}
}

func TestReview3VersionPaths(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	w := r.commitText("VERSION", "1.0.0\n", "feat: PROJ-123 version work")
	prev := w
	r.revert(w)
	head := r.head()
	active, held := partitionForTest(t, r, prev, head, w, "./VERSION")
	if len(active) != 0 || len(held) != 1 || held[0] != w {
		t.Fatalf("active %v, held %v; want normalized version-only path retained", active, held)
	}
}

func TestReview3AncestorWithVersionBumpOnlyStaysHeld(t *testing.T) {
	r := newWithheldRepo(t, "base\n")
	r.commitText("VERSION", "1.0.0\n", "chore: initial version")
	w := r.commitText("work.txt", "secret\n", "feat: PROJ-123 withheld work")
	r.revert(w)
	prev := r.head()
	head := r.commitText("VERSION", "1.0.1\n", "chore: version bump")
	paths := pathsForVersionDeploy(t, r, prev, head, config.Version{Mode: config.ModeFile, File: "VERSION"})
	active, held, err := partitionWithheld(r.repo, prev, head, []string{w}, []string{"VERSION"}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 || len(held) != 1 || held[0] != w {
		t.Fatalf("active %v, held %v; want ancestor W retained and unchecked", active, held)
	}
}

func TestPartitionWithheldModuleFileOverlap(t *testing.T) {
	tests := []struct {
		name         string
		withheld     string
		deployed     string
		withheldCode bool
		deployedCode bool
		wantActive   bool
	}{
		{
			name:     "different modules in go.mod and go.sum",
			withheld: "require example.com/liba v1.1.0",
			deployed: "require example.com/libb v1.1.0",
		},
		{
			name:       "same module overlaps",
			withheld:   "require example.com/liba v1.1.0",
			deployed:   "require example.com/liba v1.2.0",
			wantActive: true,
		},
		{
			name:     "different directive kinds do not overlap",
			withheld: "require example.com/libb v1.1.0",
			deployed: "go 1.23",
		},
		{
			name:       "same go directive overlaps",
			withheld:   "go 1.23",
			deployed:   "go 1.24",
			wantActive: true,
		},
		{
			name:         "code path still overlaps",
			withheld:     "require example.com/liba v1.1.0",
			deployed:     "require example.com/libb v1.1.0",
			withheldCode: true,
			deployedCode: true,
			wantActive:   true,
		},
		{
			name:       "malformed deployed go.mod keeps path overlap",
			withheld:   "require example.com/liba v1.1.0",
			deployed:   "require (\n",
			wantActive: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newEmptyWithheldRepo(t)
			baseMod := "module example.com/app\ngo 1.22\nrequire (\n\texample.com/liba v1.0.0\n\texample.com/libb v1.0.0\n)\n"
			baseSum := "example.com/liba v1.0.0 h1:aaaa\nexample.com/libb v1.0.0 h1:bbbb\n"
			r.writePath("go.mod", &baseMod)
			r.writePath("go.sum", &baseSum)
			r.commitAll("base modules")

			wMod := replaceModuleLine(baseMod, tc.withheld)
			r.writePath("go.mod", &wMod)
			wSum := baseSum
			if strings.HasPrefix(tc.withheld, "require example.com/liba") {
				wSum = "example.com/liba v1.1.0 h1:cccc\nexample.com/libb v1.0.0 h1:bbbb\n"
			}
			r.writePath("go.sum", &wSum)
			if tc.withheldCode {
				code := "withheld code\n"
				r.writePath("a.go", &code)
			}
			w := r.commitAll("feat: PROJ-123 withheld module change")
			r.revert(w)
			prev := r.head()

			deliveredMod := replaceModuleLine(baseMod, tc.deployed)
			if tc.name == "malformed deployed go.mod keeps path overlap" {
				deliveredMod = tc.deployed
			}
			r.writePath("go.mod", &deliveredMod)
			if strings.HasPrefix(tc.deployed, "require example.com/liba") {
				deliveredSum := "example.com/liba v1.2.0 h1:dddd\nexample.com/libb v1.0.0 h1:bbbb\n"
				r.writePath("go.sum", &deliveredSum)
			} else if strings.HasPrefix(tc.deployed, "require example.com/libb") {
				deliveredSum := "example.com/liba v1.0.0 h1:aaaa\nexample.com/libb v1.1.0 h1:eeee\n"
				r.writePath("go.sum", &deliveredSum)
			}
			if tc.deployedCode {
				code := "deployed code\n"
				r.writePath("a.go", &code)
			}
			head := r.commitAll("fix: PROJ-456 deployed module change")
			active, reverted := partitionForTest(t, r, prev, head, w)
			if tc.wantActive {
				if len(active) != 1 || active[0] != w || len(reverted) != 0 {
					t.Fatalf("active %v, reverted %v; want W active", active, reverted)
				}
			} else if len(active) != 0 || len(reverted) != 1 || reverted[0] != w {
				t.Fatalf("active %v, reverted %v; want W reverted with its marker retained", active, reverted)
			}
		})
	}
}

func TestGoModChangedKeysCollectModuleLinesAndComments(t *testing.T) {
	const header = "module example.com/app\ngo 1.22\n"
	tests := []struct {
		name     string
		before   string
		withheld string
		deployed string
	}{
		{
			name:     "require and replace of the same module overlap",
			before:   header + "require example.com/liba v1.0.0\n",
			withheld: header + "require example.com/liba v1.1.0\n",
			deployed: header + "require example.com/liba v1.0.0\nreplace example.com/liba => ../liba\n",
		},
		{
			name:     "two exclude lines retain the changed row",
			before:   header + "exclude (\n\texample.com/liba v1.0.0\n\texample.com/liba v1.1.0\n)\n",
			withheld: header + "exclude example.com/liba v1.1.0\n",
			deployed: header + "exclude (\n\texample.com/liba v1.0.0\n\texample.com/liba v1.1.1\n)\n",
		},
		{
			name:     "two replace lines retain the changed row",
			before:   header + "replace (\n\texample.com/liba v1.0.0 => ../old\n\texample.com/liba => ../current\n)\n",
			withheld: header + "replace (\n\texample.com/liba v1.0.0 => ../withheld\n\texample.com/liba => ../current\n)\n",
			deployed: header + "replace (\n\texample.com/liba v1.0.0 => ../old\n\texample.com/liba => ../deployed\n)\n",
		},
		{
			name:     "indirect comment change overlaps version bump",
			before:   header + "require example.com/liba v1.0.0\n",
			withheld: header + "require example.com/liba v1.0.0 // indirect\n",
			deployed: header + "require example.com/liba v1.1.0\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wKeys, err := goModChangedKeys([]byte(tc.before), []byte(tc.withheld))
			if err != nil {
				t.Fatal(err)
			}
			deployedKeys, err := goModChangedKeys([]byte(tc.before), []byte(tc.deployed))
			if err != nil {
				t.Fatal(err)
			}
			if !stringMapKeysIntersect(wKeys, deployedKeys) {
				t.Fatalf("withheld keys %v do not intersect deployed keys %v", wKeys, deployedKeys)
			}
		})
	}
}

func replaceModuleLine(goMod, line string) string {
	if strings.HasPrefix(line, "go ") {
		return regexp.MustCompile(`(?m)^go .*?$`).ReplaceAllString(goMod, line)
	}
	for _, existing := range []string{"\texample.com/liba v1.0.0", "\texample.com/libb v1.0.0"} {
		fields := strings.Fields(existing)
		if strings.HasPrefix(line, "require "+fields[0]) {
			return strings.Replace(goMod, existing, "\t"+strings.TrimPrefix(line, "require "), 1)
		}
	}
	return goMod
}

func pathsForVersionDeploy(t *testing.T, r *withheldRepo, prev, head string, v config.Version) map[string]bool {
	t.Helper()
	paths, err := repoPaths(r.repo, prev, head)
	if err != nil {
		t.Fatal(err)
	}
	paths, err = excludeVersionOnlyBumps(r.repo, prev, head, paths, v)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestDeployCheckPrintsWithheldVersionHintsForRelatedCommits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		withheldCount int
		ordinary      bool
	}{
		{name: "one withheld", withheldCount: 1},
		{name: "two withheld", withheldCount: 2},
		{name: "ordinary failure", ordinary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setFreezeGitConfig(t)
			issues := map[string]string{}
			var withheldKeys []string
			for i := 0; i < tc.withheldCount; i++ {
				key := fmt.Sprintf("PROJ-%d", 201+i)
				withheldKeys = append(withheldKeys, key)
				issues[key] = `"fixVersions":[{"id":"old","name":"Product 0.9"}]`
			}
			if tc.ordinary {
				issues["PROJ-203"] = `"fixVersions":[{"id":"old","name":"Product 0.9"}]`
			}
			issues["PROJ-299"] = `"fixVersions":[{"id":"target","name":"Product 1.0"}]`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasPrefix(req.URL.Path, "/rest/api/3/issue/"):
					key := strings.TrimPrefix(req.URL.Path, "/rest/api/3/issue/")
					fields, ok := issues[key]
					if !ok {
						http.NotFound(w, req)
						return
					}
					fmt.Fprintf(w, `{"key":%q,"fields":{"summary":"synthetic","issuetype":{"name":"Task"},"status":{"name":"Ready","statusCategory":{"key":"indeterminate"}},%s,"subtasks":[]}}`, key, fields)
				case strings.HasPrefix(req.URL.Path, "/rest/api/3/search/jql"):
					fmt.Fprint(w, `{"issues":[{"key":"PROJ-299"}],"isLast":true}`)
				case req.URL.Path == "/rest/api/3/version/target":
					fmt.Fprint(w, `{"id":"target","name":"Product 1.0","approvers":[{"accountId":"approver","status":"APPROVED"}]}`)
				case req.URL.Path == "/rest/api/3/user":
					fmt.Fprint(w, `{"accountId":"approver","displayName":"Approver"}`)
				default:
					t.Errorf("unexpected Jira request %s %s", req.Method, req.URL.String())
					http.NotFound(w, req)
				}
			}))
			t.Cleanup(server.Close)
			setJiraTestEnv(t, server.URL)

			repo := newWithheldRepo(t, "base\n")
			prev := repo.head()
			repo.git("checkout", "-qb", "withheld", prev)
			var withheldSHAs []string
			for i, key := range withheldKeys {
				withheldSHAs = append(withheldSHAs, repo.commitText(fmt.Sprintf("withheld-%d.txt", i), "held\n", "feat: "+key+" withheld work"))
			}
			repo.git("checkout", "-qb", "deploy", prev)
			repo.commitText("candidate.txt", "candidate\n", "feat: PROJ-299 release work")
			if tc.ordinary {
				repo.commitText("ordinary.txt", "ordinary\n", "feat: PROJ-203 ordinary work")
			}
			head := repo.head()
			marker := filepath.Join(t.TempDir(), "deployed_sha")
			if err := os.WriteFile(marker, []byte(prev+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker+withheldSuffix, []byte(strings.Join(withheldSHAs, "\n")), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{
				Version:      config.Version{Mode: config.ModeNone},
				Envs:         map[string]*config.Env{"local": {}},
				ReleaseNotes: &config.ReleaseNotes{Jira: &config.JiraNotes{ProjectKeys: []string{"PROJ"}}},
				Deploy:       &config.Deploy{Targets: map[string]*config.DeployTarget{}},
			}
			target := &config.DeployTarget{
				Env: "local", DeployedSHA: &config.DeployedSHA{Path: marker},
				ApprovedRelease: &config.ApprovedRelease{Statuses: []string{"Ready"}, Version: &config.ApprovedReleaseVersion{Match: "^Product 1\\."}},
			}
			cfg.Deploy.Targets["prod"] = target
			env, err := envs.Get(cfg, repo.dir, "local")
			if err != nil {
				t.Fatal(err)
			}
			run := &deployRun{name: "prod", cfg: cfg, repo: repo.repo, target: target, env: env, prev: prev, head: head, dryRun: true}
			err = (&App{Stdout: io.Discard, Stderr: io.Discard}).checkApproved(t.Context(), run, false)
			if err == nil || !strings.Contains(err.Error(), "version-top") {
				t.Fatalf("deploy check error = %v; want version-top refusal", err)
			}
			for _, sha := range withheldSHAs {
				want := gitx.Short(sha) + " was withheld from an earlier release: if its change is already on the target, remove its line from " + marker + withheldSuffix + "."
				if got := strings.Count(err.Error(), want); got != 1 {
					t.Fatalf("deploy check has %d hints for withheld %s; want one:\n%v", got, gitx.Short(sha), err)
				}
			}
			if tc.ordinary {
				if strings.Contains(err.Error(), "ordinary.txt") || strings.Contains(err.Error(), "withheld from an earlier release") {
					t.Fatalf("ordinary commit refusal unexpectedly names a withheld marker: %v", err)
				}
			}
		})
	}
}
