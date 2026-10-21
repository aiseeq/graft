package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/deploy"
	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// checkIntegrations compares the integrations before deps and the sudo
// prompt, or, when the command asks the uploaded build, leaves it to the
// deploy script and reads the exceptions now so a bad file fails early.
func (a *App) checkIntegrations(ctx context.Context, r *deployRun) error {
	in := r.target.Integrations
	if in == nil {
		return nil
	}
	if !in.Staged() {
		if err := a.compareIntegrations(ctx, r.repo, r.cfg, r.name, ""); err != nil {
			return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
		}
		return nil
	}
	if _, err := deploy.ReadIntegrationExceptions(exceptionsPath(r.repo, in)); err != nil {
		return fmt.Errorf("deploy %s: nothing was deployed: %w", r.name, err)
	}
	if err := os.Remove(integrationsMarker(r.repo, r.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a.printf("the deploy script compares the integrations with %s: graft deploy check-integrations <build>", r.target.Requires)
	return nil
}

// requireIntegrationsChecked refuses to record a deploy whose script never
// passed graft deploy check-integrations.
func requireIntegrationsChecked(r *deployRun) error {
	if r.target.Integrations == nil || !r.target.Integrations.Staged() {
		return nil
	}
	got, err := os.ReadFile(integrationsMarker(r.repo, r.name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.TrimSpace(string(got)) != r.head {
		return fmt.Errorf("the deploy script finished, but it never passed graft deploy check-integrations <build> for %s; the deployed commit is not recorded", gitx.Short(r.head))
	}
	return nil
}

// DeployCheckIntegrations is graft deploy check-integrations: the deploy
// script calls it once the build is uploaded, with the path {staged} takes.
func (a *App) DeployCheckIntegrations(ctx context.Context, staged string) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	name, sha := os.Getenv(EnvDeployTarget), os.Getenv(EnvDeploySHA)
	if name == "" || sha == "" {
		return fmt.Errorf("%s and %s are not set: check-integrations runs inside graft deploy", EnvDeployTarget, EnvDeploySHA)
	}
	if cfg.Deploy == nil || cfg.Deploy.Targets[name] == nil || cfg.Deploy.Targets[name].Integrations == nil {
		return fmt.Errorf("target %q has no integrations section", name)
	}
	switch in := cfg.Deploy.Targets[name].Integrations; {
	case in.Staged() && staged == "":
		return fmt.Errorf("the integrations command of %s asks %s: pass the uploaded build, graft deploy check-integrations <build>", name, config.StagedPlaceholder)
	case !in.Staged() && staged != "":
		return fmt.Errorf("the integrations command of %s has no %s: graft deploy compares them itself, before deps", name, config.StagedPlaceholder)
	}
	if err := a.compareIntegrations(ctx, repo, cfg, name, staged); err != nil {
		return err
	}
	marker := integrationsMarker(repo, name)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return fmt.Errorf("the integrations marker: %w", err)
	}
	return os.WriteFile(marker, []byte(sha+"\n"), 0o644)
}

// compareIntegrations refuses when the target from requires runs an
// integration the named target does not configure and no exception excuses.
func (a *App) compareIntegrations(ctx context.Context, repo *gitx.Repo, cfg *config.Config, name, staged string) error {
	t := cfg.Deploy.Targets[name]
	in := t.Integrations
	exceptions, err := deploy.ReadIntegrationExceptions(exceptionsPath(repo, in))
	if err != nil {
		return err
	}
	read := func(target, staged string) ([]string, error) {
		env, err := envs.Get(cfg, repo.Root, cfg.Deploy.Targets[target].Env)
		if err != nil {
			return nil, err
		}
		return deploy.Integrations(ctx, env, in.Command, staged)
	}
	required, err := read(t.Requires, in.Running)
	if err != nil {
		return err
	}
	here, err := read(name, staged)
	if err != nil {
		return err
	}
	missing, excused, stale := deploy.IntegrationGaps(required, here, exceptions)
	for _, n := range excused {
		a.printf("%s runs %s, %s leaves it off: %s", t.Requires, n, name, exceptions[n])
	}
	for _, n := range stale {
		a.warn("the exception for %s in %s is not needed any more: drop the line", n, in.Exceptions)
	}
	if len(missing) > 0 {
		excuse := "list it in " + in.Exceptions + " with a reason"
		if in.Exceptions == "" {
			excuse = "list it in an integrations.exceptions file as \"integration|reason\""
		}
		return fmt.Errorf("%s runs %s, which %s does not configure: set it up on %s or %s", t.Requires, strings.Join(missing, ", "), name, name, excuse)
	}
	a.printf("%s configures every integration %s runs (%s: %s)", name, t.Requires, name, strings.Join(here, ", "))
	return nil
}

func exceptionsPath(repo *gitx.Repo, in *config.Integrations) string {
	if in.Exceptions == "" {
		return ""
	}
	return filepath.Join(repo.Root, filepath.FromSlash(in.Exceptions))
}

// integrationsMarker records that check-integrations passed for a commit.
func integrationsMarker(repo *gitx.Repo, target string) string {
	return filepath.Join(repo.CommonDir, "graft-deploy", target+".integrations")
}
