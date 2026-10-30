package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// DeployCheck is graft deploy check: the checks graft deploy runs before the
// deps and the script, read only, every refusal listed. Nothing is deployed
// or recorded, and no sudo password is asked for.
func (a *App) DeployCheck(ctx context.Context, target string) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	r.dryRun = true
	var refusals []error
	refuse := func(err error) {
		if err != nil {
			refusals = append(refusals, err)
		}
	}
	refuse(deploy.CheckTree(r.repo, r.cfg.Deploy))
	if r.head, err = deploy.PublishedHead(r.repo, r.cfg.Deploy); err != nil {
		refuse(err)
		head, err := r.repo.Git("rev-parse", "HEAD")
		if err != nil {
			return err
		}
		r.head = strings.TrimSpace(head)
	}
	if r.cfg.Version.Mode != config.ModeNone {
		if r.version, err = projectVersion(r.repo, r.cfg); err != nil {
			return err
		}
	}
	a.printf("checking graft deploy %s for %s (version %s) on %s; nothing is deployed", r.name, gitx.Short(r.head), r.version, r.env.Destination())
	if r.now, err = deployNow(); err != nil {
		return err
	}
	if r.branch, err = r.repo.Branch(); err != nil {
		return err
	}
	if lookup, lookupErr := activeFreezeForRun(r); lookupErr != nil {
		return lookupErr
	} else {
		r.freeze = lookup.freeze
	}
	if r.hotfix = r.cfg.Deploy.IsHotfix(r.branch); !r.hotfix {
		refuse(a.checkWindow(r))
	}
	refuse(a.requireDeployed(ctx, r))
	if r.target.DeployedSHA == nil {
		if err := a.showDeployed(ctx, r); err != nil {
			return err
		}
	} else if r.prev, err = r.readSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
		refuse(err)
	} else if err := a.showDeployed(ctx, r); err != nil {
		return err
	} else if r.prev == r.head {
		refuse(fmt.Errorf("%s already runs %s; graft deploy --redeploy %s deploys it again", r.name, gitx.Short(r.head), r.name))
	} else {
		refuse(a.checkReleaseFlow(ctx, r, false))
	}
	// Freeze refusals are useful even when an earlier check already failed.
	refuse(a.checkFreeze(ctx, r, DeployOptions{}))
	if len(refusals) == 0 {
		a.printf("graft deploy %s passes its checks now; its deps, the integrations and the confirmation run only in the deploy", r.name)
		return nil
	}
	fmt.Fprintf(a.Stdout, "\ngraft deploy %s would refuse:\n", r.name)
	for _, err := range refusals {
		fmt.Fprintf(a.Stdout, "- %s\n", strings.ReplaceAll(err.Error(), "\n", "\n  "))
	}
	return fmt.Errorf("graft deploy %s would refuse, see above", r.name)
}

// DeployVersions is graft deploy versions: the version and the commit each
// target runs, read without a sudo password.
func (a *App) DeployVersions(ctx context.Context) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	if cfg.Deploy == nil {
		return errors.New("deploy is not configured: add a deploy section to .graft.yaml")
	}
	var failed []string
	for _, name := range targetOrder(cfg.Deploy.Targets) {
		t := cfg.Deploy.Targets[name]
		env, err := envs.Get(cfg, repo.Root, t.Env)
		if err != nil {
			return err
		}
		var sha string
		var shaErr error
		if t.DeployedSHA != nil {
			sha, shaErr = deploy.PeekDeployedSHA(ctx, env, t.DeployedSHA)
		}
		s, err := readDeployed(ctx, repo, cfg, env, t, sha)
		if err != nil {
			return err
		}
		if shaErr != nil {
			s.commit, s.failed = "unknown: "+shaErr.Error(), true
		}
		if s.failed {
			failed = append(failed, name)
		}
		fmt.Fprintf(a.Stdout, "%s (%s): version %s; commit %s\n", name, env.Destination(), s.version, s.commit)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not read %s", strings.Join(slices.Compact(failed), ", "))
	}
	return nil
}

// deployedState is what a target runs: its version and its commit, each
// read or said why not; failed tells that one could not be read.
type deployedState struct {
	version, commit string
	failed          bool
}

// readDeployed describes what a target runs, sha being the commit its
// deployed_sha records (empty: none recorded, or no deployed_sha). The
// version comes from the target's version command, or else from the version
// files of that commit; the commit is described with the remote branches it
// is on.
func readDeployed(ctx context.Context, repo *gitx.Repo, cfg *config.Config, env *envs.Env, t *config.DeployTarget, sha string) (deployedState, error) {
	s := deployedState{version: "no version command", commit: "no deployed_sha"}
	known := false
	if t.DeployedSHA != nil {
		s.commit = "none recorded"
		if sha != "" {
			var err error
			if known, err = repo.RevExists(sha); err != nil {
				return s, err
			}
			if s.commit, err = describeDeployedCommit(repo, sha, known); err != nil {
				return s, err
			}
		}
	}
	switch {
	case t.Version.IsSet():
		v, err := deploy.DeployedVersion(ctx, env, t.Version)
		if err != nil {
			s.version, s.failed = "unknown: "+err.Error(), true
		} else {
			s.version = v
		}
	case known && cfg.Version.Mode != config.ModeNone:
		v, err := versionAt(repo, cfg, sha)
		if err != nil {
			s.version, s.failed = "unknown: "+err.Error(), true
		} else {
			s.version = v + " by its commit"
		}
	}
	return s, nil
}

// describeDeployedCommit is the commit a target runs, from the local
// repository: subject, date and the remote branches at it, or else those
// that contain it.
func describeDeployedCommit(repo *gitx.Repo, sha string, known bool) (string, error) {
	if !known {
		return gitx.Short(sha) + ", not in the local repository (git fetch?)", nil
	}
	line, err := repo.Git("log", "-1", "--format=%h %s (%cs)", sha)
	if err != nil {
		return "", err
	}
	branches, err := remoteBranches(repo, sha)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line) + ", " + branches, nil
}

// remoteBranches says which remote branches are at sha, or else contain it,
// three named at most.
func remoteBranches(repo *gitx.Repo, sha string) (string, error) {
	for _, how := range []struct{ flag, word string }{{"--points-at", "at"}, {"--contains", "in"}} {
		out, err := repo.Git("for-each-ref", how.flag, sha, "--format=%(refname:short)", "refs/remotes")
		if err != nil {
			return "", err
		}
		var names []string
		for n := range strings.FieldsSeq(out) {
			// refs/remotes/<remote>/HEAD is short for the remote alone.
			if strings.Contains(n, "/") && !strings.HasSuffix(n, "/HEAD") {
				names = append(names, n)
			}
		}
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
		}
		if len(names) > 0 {
			return how.word + " " + strings.Join(names, ", "), nil
		}
	}
	return "on no remote branch", nil
}

// targetOrder lists targets after the ones they require, by name otherwise:
// test, then uat, then prod.
func targetOrder(targets map[string]*config.DeployTarget) []string {
	depth := func(name string) int {
		n := 0
		for seen := map[string]bool{}; targets[name] != nil && targets[name].Requires != "" && !seen[name]; n++ {
			seen[name] = true
			name = targets[name].Requires
		}
		return n
	}
	return slices.SortedFunc(maps.Keys(targets), func(x, y string) int {
		return cmp.Or(cmp.Compare(depth(x), depth(y)), cmp.Compare(x, y))
	})
}
