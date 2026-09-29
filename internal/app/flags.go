package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/aiseeq/graft/internal/checks"
	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/envs"
	"github.com/aiseeq/graft/internal/flags"
	"github.com/aiseeq/graft/internal/gitx"
)

// FlagsOptions are the arguments of graft flags.
type FlagsOptions struct {
	Command string // status, show, ack or mute
	Env     string // empty means flags.default_env
	ID      string
	Reason  string
	Match   string
	AllEnvs bool
}

// Flags runs a graft flags subcommand.
func (a *App) Flags(ctx context.Context, o FlagsOptions) error {
	repo, cfg, err := a.open()
	if err != nil {
		return err
	}
	j, err := a.journal(repo, cfg, o.Env)
	if err != nil {
		return err
	}
	switch o.Command {
	case "status":
		return j.Status(ctx)
	case "show":
		return j.Show(ctx, o.ID)
	case "ack":
		return j.Ack(ctx, o.ID, o.Reason)
	case "mute":
		return j.Mute(ctx, flags.MuteOptions{ID: o.ID, Reason: o.Reason, Match: o.Match, AllEnvs: o.AllEnvs})
	default:
		return fmt.Errorf("unknown flags command %q", o.Command)
	}
}

func (a *App) journal(repo *gitx.Repo, cfg *config.Config, envName string) (*flags.Journal, error) {
	f := cfg.Flags
	if f == nil {
		return nil, errors.New("flags are not configured: add a flags section to .graft.yaml")
	}
	if envName == "" {
		envName = f.DefaultEnv
	}
	env, err := envs.Get(cfg, repo.Root, envName)
	if err != nil {
		return nil, err
	}
	var source flags.Source
	if f.SQL != nil {
		source, err = flags.NewSQLSource(f.SQL, env)
	} else {
		source, err = flags.NewHTTPSource(f.HTTP, env)
	}
	if err != nil {
		return nil, err
	}
	rulesPath := filepath.Join(repo.Root, filepath.FromSlash(f.Exceptions))
	rules, err := flags.LoadRules(rulesPath, cfg.EnvNames())
	if err != nil {
		return nil, err
	}
	return &flags.Journal{Env: envName, Source: source, Rules: rules, RulesPath: rulesPath, KnownEnvs: cfg.EnvNames(), Out: a.Stdout}, nil
}

// flagRuleFindings validates the staged exceptions file.
func flagRuleFindings(repo *gitx.Repo, cfg *config.Config) ([]checks.Finding, error) {
	if cfg.Flags == nil {
		return []checks.Finding{}, nil
	}
	blobs, err := repo.ReadBlobs([]string{":" + cfg.Flags.Exceptions}, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("flags exceptions file %s: %w", cfg.Flags.Exceptions, err)
	}
	if _, err := flags.ParseRules(blobs[0].Content, cfg.EnvNames()); err != nil {
		return []checks.Finding{{Path: cfg.Flags.Exceptions, Reason: err.Error()}}, nil
	}
	return []checks.Finding{}, nil
}
