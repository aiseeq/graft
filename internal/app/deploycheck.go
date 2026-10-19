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
	if r.hotfix = r.cfg.Deploy.IsHotfix(r.branch); !r.hotfix {
		refuse(a.checkWindow(r))
	}
	refuse(a.requireDeployed(ctx, r))
	if r.target.DeployedSHA != nil {
		if r.prev, err = r.readSHA(ctx, r.env, r.target.DeployedSHA); err != nil {
			refuse(err)
		} else if r.prev == r.head {
			refuse(fmt.Errorf("%s already runs %s; graft deploy --redeploy %s deploys it again", r.name, gitx.Short(r.head), r.name))
		} else {
			refuse(a.checkReleaseFlow(ctx, r, false))
		}
	}
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
		version, commit := "no version command", "no deployed_sha"
		if t.Version.IsSet() {
			if version, err = deploy.DeployedVersion(ctx, env, t.Version); err != nil {
				version, failed = "unknown: "+err.Error(), append(failed, name)
			}
		}
		if t.DeployedSHA != nil {
			if commit, err = deployedCommit(ctx, repo, env, t.DeployedSHA); err != nil {
				commit, failed = "unknown: "+err.Error(), append(failed, name)
			}
		}
		fmt.Fprintf(a.Stdout, "%s (%s): version %s; commit %s\n", name, env.Destination(), version, commit)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not read %s", strings.Join(slices.Compact(failed), ", "))
	}
	return nil
}

// deployedCommit describes the commit a target runs from the local
// repository.
func deployedCommit(ctx context.Context, repo *gitx.Repo, env *envs.Env, rec *config.DeployedSHA) (string, error) {
	sha, err := deploy.PeekDeployedSHA(ctx, env, rec)
	if err != nil {
		return "", err
	}
	if sha == "" {
		return "none recorded", nil
	}
	known, err := repo.RevExists(sha)
	if err != nil {
		return "", err
	}
	if !known {
		return gitx.Short(sha) + ", not in the local repository (git fetch?)", nil
	}
	line, err := repo.Git("log", "-1", "--format=%h %s (%cs)", sha)
	return strings.TrimSpace(line), err
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
