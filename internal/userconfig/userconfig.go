// Package userconfig reads the per-user graft settings in
// <user config dir>/graft/config.yaml: things that belong to the person, not
// to a project, such as where their tracker credentials live.
package userconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config is the user's graft settings.
type Config struct {
	Jira  Jira  `yaml:"jira"`
	Tools Tools `yaml:"tools"`
}

// Tools says where graft installs pinned tools. Empty BinDir leaves it to go
// install (GOBIN, else GOPATH/bin).
type Tools struct {
	BinDir string `yaml:"bin_dir"`
}

// Jira says where the Jira credentials are: an env file with JIRA_BASE_URL,
// JIRA_EMAIL and JIRA_API_TOKEN.
type Jira struct {
	EnvFile string `yaml:"env_file"`
}

// Path returns the user config file location.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "graft", "config.yaml"), nil
}

// Load reads the user config. A missing file is ErrNotFound: callers decide
// whether they need it.
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, path, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Tools.BinDir != "" {
		if cfg.Tools.BinDir, err = expandHome(cfg.Tools.BinDir); err != nil {
			return nil, path, err
		}
		if !filepath.IsAbs(cfg.Tools.BinDir) {
			return nil, path, fmt.Errorf("%s: tools.bin_dir must be absolute or start with ~/", path)
		}
	}
	if cfg.Jira.EnvFile != "" {
		if cfg.Jira.EnvFile, err = expandHome(cfg.Jira.EnvFile); err != nil {
			return nil, path, err
		}
	}
	return &cfg, path, nil
}

// ErrNotFound means the user has no graft config file.
var ErrNotFound = errors.New("user config not found")

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
