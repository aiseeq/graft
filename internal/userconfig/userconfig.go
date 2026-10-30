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
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/akit/pressure"
	"go.yaml.in/yaml/v3"

	"github.com/aiseeq/graft/v2/internal/config"
)

// Config is the user's graft settings.
type Config struct {
	Jira  Jira   `yaml:"jira"`
	Tools Tools  `yaml:"tools"`
	Leaks *Leaks `yaml:"leaks"`
	// Load is how this machine takes heavy tasks.
	Load MachineLoad `yaml:"load"`
	// Limits are how long the gate and a task may take.
	Limits Limits `yaml:"limits"`
	// Commit is what graft commit asks of the user's messages.
	Commit Commit `yaml:"commit"`
	// Modules names module path prefixes graft keeps at their latest release.
	Modules Modules `yaml:"modules"`
	// RemoteHeavy is the host heavy work of every project overflows to, when
	// the project names none of its own; it defaults to when: busy.
	RemoteHeavy *config.RemoteHeavy `yaml:"remote_heavy"`
	// Install is where the install step copies what it installs here.
	Install Install `yaml:"install"`
}

// Install names hosts that receive binaries installed by an install step.
type Install struct {
	Hosts []string `yaml:"hosts"`
}

// RemoteHeavySettings returns the user's remote host when configured.
func RemoteHeavySettings() (*config.RemoteHeavy, error) {
	cfg, _, err := Load()
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cfg.RemoteHeavy, nil
}

// Modules lists Go module path prefixes whose requirements graft raises to
// their latest published versions before a commit or a manual gate.
type Modules struct {
	Latest []string `yaml:"latest"`
}

// IsLatest reports whether path is equal to a configured prefix or below it.
func (m Modules) IsLatest(path string) bool {
	for _, prefix := range m.Latest {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix != "" && (path == prefix || strings.HasPrefix(path, prefix+"/")) {
			return true
		}
	}
	return false
}

// ModulesSettings is the user's module settings; without a user config none
// are enabled.
func ModulesSettings() (Modules, error) {
	cfg, _, err := Load()
	if errors.Is(err, ErrNotFound) {
		return Modules{}, nil
	}
	if err != nil {
		return Modules{}, err
	}
	return cfg.Modules, nil
}

// Commit is what graft commit asks of the messages in every project of the
// user.
type Commit struct {
	// Conventional asks for a Conventional Commits subject.
	Conventional bool `yaml:"conventional"`
	// LocalBranches are branch globs whose commits stay on this machine
	// until someone merges them: a plain git commit there passes the hook
	// after the checks, with no gate, no version bump and no push.
	LocalBranches []string `yaml:"local_branches"`
}

// CommitSettings is the user's commit settings; without a user config none
// is asked for.
func CommitSettings() (Commit, error) {
	cfg, _, err := Load()
	if errors.Is(err, ErrNotFound) {
		return Commit{}, nil
	}
	if err != nil {
		return Commit{}, err
	}
	return cfg.Commit, nil
}

// Limits are the times graft stats marks a gate or a task over, measured
// without the waits for the load and the locks: the median run of the
// period.
type Limits struct {
	Gate time.Duration `yaml:"gate"`
	Task time.Duration `yaml:"task"`
}

// DefaultLimits keep the gate of a commit under a minute and a test run
// before a deploy, merge or release under 8 minutes.
var DefaultLimits = Limits{Gate: time.Minute, Task: 8 * time.Minute}

// LimitSettings is the user's limits over the defaults; without a user
// config it is the defaults.
func LimitSettings() (Limits, error) {
	cfg, _, err := Load()
	if errors.Is(err, ErrNotFound) {
		return DefaultLimits, nil
	}
	if err != nil {
		return Limits{}, err
	}
	return cfg.Limits, nil
}

func (l *Limits) normalize(path string) error {
	if l.Gate < 0 || l.Task < 0 {
		return fmt.Errorf("%s: limits: gate and task must be positive", path)
	}
	if l.Gate == 0 {
		l.Gate = DefaultLimits.Gate
	}
	if l.Task == 0 {
		l.Task = DefaultLimits.Task
	}
	return nil
}

// MachineLoad is when a heavy task may start on this machine: the tasks
// ready to run below WaitAbove, the IO pressure at most MaxIOFull, and not
// the memory pressure above MaxMemSome while it swaps more than MaxSwapKBs.
// Heavy tasks start one at a time, each giving
// its load Settle to show before the next one measures the machine.
type MachineLoad struct {
	// WaitAbove 0 is the machine's thread count.
	WaitAbove float64 `yaml:"wait_above"`
	// MaxIOFull is the share of the last 5 seconds, in percent, during which
	// every running process stalled on the disk (/proc/pressure/io), at or
	// above which the machine is busy; a disk freeze going on makes it busy
	// too. 100 or more leaves the disk out.
	MaxIOFull float64 `yaml:"max_io_full"`
	// MaxMemSome is the share of the last 10 seconds, in percent, during
	// which some process stalled on memory (/proc/pressure/memory); 0 is the
	// rule's own default.
	MaxMemSome float64 `yaml:"max_mem_some"`
	// MaxSwapKBs is the swap traffic in and out, in KB per second, measured
	// over SwapWindow: swap in use is no harm, swapping is.
	MaxSwapKBs float64       `yaml:"max_swap_kbs"`
	SwapWindow time.Duration `yaml:"swap_window"`
	Threads    int           `yaml:"threads"`
	Settle     time.Duration `yaml:"settle"`
	Poll       time.Duration `yaml:"poll"`
}

// DefaultLoad is a shared machine's: heavy tasks start while fewer tasks are
// ready to run than it has threads, with the disk and the swap calm. The
// count is of this moment, so a start shows to the next one within seconds.
var DefaultLoad = MachineLoad{MaxIOFull: pressure.DefaultLimits.MaxIOFull, MaxSwapKBs: 1024, SwapWindow: time.Second,
	Threads: 4, Settle: 10 * time.Second, Poll: 5 * time.Second}

// LoadSettings is the user's load settings over the defaults; without a user
// config it is the defaults.
func LoadSettings() (MachineLoad, error) {
	cfg, _, err := Load()
	if errors.Is(err, ErrNotFound) {
		return DefaultLoad, nil
	}
	if err != nil {
		return MachineLoad{}, err
	}
	return cfg.Load, nil
}

func (l *MachineLoad) normalize(path string) error {
	d := DefaultLoad
	for _, f := range []struct {
		v   *float64
		def float64
	}{{&l.MaxIOFull, d.MaxIOFull}, {&l.MaxSwapKBs, d.MaxSwapKBs}} {
		if *f.v == 0 {
			*f.v = f.def
		}
	}
	if l.Threads == 0 {
		l.Threads = d.Threads
	}
	if l.Settle == 0 {
		l.Settle = d.Settle
	}
	if l.SwapWindow == 0 {
		l.SwapWindow = d.SwapWindow
	}
	if l.Poll == 0 {
		l.Poll = d.Poll
	}
	switch {
	case l.WaitAbove < 0 || l.MaxIOFull < 0 || l.MaxMemSome < 0 || l.MaxSwapKBs < 0:
		return fmt.Errorf("%s: load: wait_above, max_io_full, max_mem_some and max_swap_kbs must be positive", path)
	case l.Threads < 0:
		return fmt.Errorf("%s: load.threads: %d is negative", path, l.Threads)
	case l.Settle < 0 || l.Poll < 0 || l.SwapWindow < 0:
		return fmt.Errorf("%s: load: settle, poll and swap_window must be positive", path)
	}
	return nil
}

// Leaks keeps the user's private names out of their public repositories. The
// lists stay here, outside any repository.
type Leaks struct {
	// PublicRemotes are remote URL prefixes, host/path form
	// (github.com/someone/): a repository pushing to one is public.
	PublicRemotes []string `yaml:"public_remotes"`
	// PrivateRemotes are remote URL prefixes in the same form that make a
	// repository private even under a public prefix: the narrower prefix
	// wins (github.com/someone/secret-tool under github.com/someone/).
	PrivateRemotes []string `yaml:"private_remotes"`
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
	if err := cfg.Limits.normalize(path); err != nil {
		return nil, path, err
	}
	if err := cfg.Load.normalize(path); err != nil {
		return nil, path, err
	}
	if cfg.RemoteHeavy != nil {
		if err := cfg.RemoteHeavy.Normalize("busy"); err != nil {
			return nil, path, err
		}
	}
	return &cfg, path, nil
}

func (l *Leaks) normalize(path string) error {
	if len(l.PublicRemotes) == 0 {
		return fmt.Errorf("%s: leaks.public_remotes: required", path)
	}
	if err := checkRemotePrefixes(path, "public_remotes", l.PublicRemotes); err != nil {
		return err
	}
	if err := checkRemotePrefixes(path, "private_remotes", l.PrivateRemotes); err != nil {
		return err
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

func checkRemotePrefixes(path, key string, prefixes []string) error {
	for i, p := range prefixes {
		if strings.Contains(p, "://") || strings.Contains(p, "@") {
			return fmt.Errorf("%s: leaks.%s[%d]: %q must be host/path like github.com/someone/", path, key, i, p)
		}
	}
	return nil
}

// RemoteIsPublic reports whether a remote URL falls under one of the public
// prefixes and under none of the private ones. scp-like (git@host:path) and
// URL forms compare as host/path.
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
	under := func(prefixes []string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(norm, p) })
	}
	return under(l.PublicRemotes) && !under(l.PrivateRemotes)
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
