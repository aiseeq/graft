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
	Jira  Jira   `yaml:"jira"`
	Tools Tools  `yaml:"tools"`
	Leaks *Leaks `yaml:"leaks"`
}

// Leaks keeps the user's private names out of their public repositories. The
// lists stay here, outside any repository.
type Leaks struct {
	// PublicRemotes are remote URL prefixes, host/path form
	// (github.com/someone/): a repository pushing to one is public.
	PublicRemotes []string `yaml:"public_remotes"`
	// PrivateSources are private project directories; the Go identifiers
	// they declare are private names.
	PrivateSources []string `yaml:"private_sources"`
	// TermsFile lists private terms, "kind: regexp" per line.
	TermsFile string `yaml:"terms_file"`
	// MinNameLength drops shorter identifiers; DefaultMinNameLength if 0.
	MinNameLength int `yaml:"min_name_length"`
	// Allow exempts a name or a term match everywhere.
	Allow []LeakAllow `yaml:"allow"`
}

// LeakAllow exempts one name or term match. The reason is mandatory.
type LeakAllow struct {
	Term   string `yaml:"term"`
	Reason string `yaml:"reason"`
}

// DefaultMinNameLength keeps camel-case names of two short words and longer.
const DefaultMinNameLength = 10

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
		return "", fmt.Errorf("locating the user config: %w", err)
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
		return nil, path, fmt.Errorf("reading the user config: %w", err)
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
	if cfg.Leaks != nil {
		if err := cfg.Leaks.normalize(path); err != nil {
			return nil, path, err
		}
	}
	return &cfg, path, nil
}

func (l *Leaks) normalize(path string) error {
	if len(l.PublicRemotes) == 0 {
		return fmt.Errorf("%s: leaks.public_remotes: required", path)
	}
	for i, p := range l.PublicRemotes {
		if strings.Contains(p, "://") || strings.Contains(p, "@") {
			return fmt.Errorf("%s: leaks.public_remotes[%d]: %q must be host/path like github.com/someone/", path, i, p)
		}
	}
	if len(l.PrivateSources) == 0 && l.TermsFile == "" {
		return fmt.Errorf("%s: leaks: set private_sources, terms_file or both", path)
	}
	for i, src := range l.PrivateSources {
		abs, err := expandHome(src)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(abs) {
			return fmt.Errorf("%s: leaks.private_sources[%d] must be absolute or start with ~/", path, i)
		}
		l.PrivateSources[i] = abs
	}
	if l.TermsFile != "" {
		abs, err := expandHome(l.TermsFile)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(abs) {
			return fmt.Errorf("%s: leaks.terms_file must be absolute or start with ~/", path)
		}
		l.TermsFile = abs
	}
	if l.MinNameLength == 0 {
		l.MinNameLength = DefaultMinNameLength
	}
	if l.MinNameLength < 0 {
		return fmt.Errorf("%s: leaks.min_name_length must be positive", path)
	}
	for i, a := range l.Allow {
		if a.Term == "" {
			return fmt.Errorf("%s: leaks.allow[%d].term: required", path, i)
		}
		if strings.TrimSpace(a.Reason) == "" {
			return fmt.Errorf("%s: leaks.allow[%d] (%s): reason is required", path, i, a.Term)
		}
	}
	return nil
}

// RemoteIsPublic reports whether a remote URL falls under one of the public
// prefixes. scp-like (git@host:path) and URL forms compare as host/path.
func (l *Leaks) RemoteIsPublic(url string) bool {
	norm := url
	if _, rest, ok := strings.Cut(norm, "://"); ok {
		norm = rest
	} else if host, p, ok := strings.Cut(norm, ":"); ok && !strings.Contains(host, "/") {
		norm = host + "/" + p
	}
	if at := strings.Index(norm, "@"); at >= 0 && at < strings.Index(norm+"/", "/") {
		norm = norm[at+1:]
	}
	for _, p := range l.PublicRemotes {
		if strings.HasPrefix(norm, p) {
			return true
		}
	}
	return false
}

// ErrNotFound means the user has no graft config file.
var ErrNotFound = errors.New("user config not found")

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %s: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}
