// Package config loads and validates .graft.yaml, the per-project description
// of how graft commits, versions and checks a repository.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// FileName is the config file graft reads from the work tree root.
const FileName = ".graft.yaml"

// SchemaVersion is the only config schema this build understands.
const SchemaVersion = 1

// Version modes.
const (
	ModeFile   = "file"
	ModeGitTag = "git-tag"
	ModeNone   = "none"
)

// Formats of files that carry a copy of the version.
const (
	FormatPlain = "plain"
	FormatJSON  = "json"
	FormatRegex = "regex"
)

// Defaults applied to fields the config leaves out.
const (
	DefaultVersionFile    = "VERSION"
	DefaultTagPrefix      = "v"
	DefaultHooksDir       = ".githooks"
	DefaultLockTimeout    = 15 * time.Minute
	DefaultMaxBinaryBytes = 1 << 20
	DefaultMaxTextBytes   = 5 << 20
)

// DefaultBinaryExtensions are file types for which binary content is expected.
var DefaultBinaryExtensions = []string{
	"png", "jpg", "jpeg", "gif", "webp", "ico", "svgz",
	"woff", "woff2", "ttf", "otf", "eot", "pdf", "mp4", "webm", "wasm",
}

// Config is a validated .graft.yaml with defaults applied.
type Config struct {
	Schema  int       `yaml:"schema"`
	Gate    []Command `yaml:"gate"`
	Version Version   `yaml:"version"`
	Ticket  *Ticket   `yaml:"ticket"`
	Push    Push      `yaml:"push"`
	Checks  Checks    `yaml:"checks"`
	Hooks   Hooks     `yaml:"hooks"`
	Lock    Lock      `yaml:"lock"`

	Envs   map[string]*Env `yaml:"envs"`
	DotEnv string          `yaml:"dotenv"`
	Flags  *Flags          `yaml:"flags"`
}

// Version describes where the project version lives.
type Version struct {
	Mode        string     `yaml:"mode"`
	File        string     `yaml:"file"`
	TagPrefix   string     `yaml:"tag_prefix"`
	TagOnCommit bool       `yaml:"tag_on_commit"`
	Sync        []SyncFile `yaml:"sync"`
}

// SyncFile is a file that repeats the version and is rewritten on every bump.
type SyncFile struct {
	Path    string   `yaml:"path"`
	Format  string   `yaml:"format"`
	Key     []string `yaml:"key"`
	Pattern string   `yaml:"pattern"`

	// Regexp is Pattern compiled; set for the regex format.
	Regexp *regexp.Regexp `yaml:"-"`
}

// Ticket describes how a work item key is recognised in a branch name.
type Ticket struct {
	Pattern string         `yaml:"pattern"`
	Regexp  *regexp.Regexp `yaml:"-"`
}

// Push selects the remotes a commit is published to.
type Push struct {
	// Remotes to push to; empty means every configured remote.
	Remotes []string `yaml:"remotes"`
}

// Checks configures the content checks run on the staged changes.
type Checks struct {
	Secrets    Secrets    `yaml:"secrets"`
	LargeFiles LargeFiles `yaml:"large_files"`
}

// Secrets configures the credential scan.
type Secrets struct {
	Enabled       *bool       `yaml:"enabled"`
	ExtraPatterns []string    `yaml:"extra_patterns"`
	Exceptions    []Exception `yaml:"exceptions"`

	Extra []*regexp.Regexp `yaml:"-"`
}

// LargeFiles configures the binary and file size check.
type LargeFiles struct {
	Enabled          *bool       `yaml:"enabled"`
	MaxBinaryBytes   int64       `yaml:"max_binary_bytes"`
	MaxTextBytes     int64       `yaml:"max_text_bytes"`
	BinaryExtensions []string    `yaml:"binary_extensions"`
	Exceptions       []Exception `yaml:"exceptions"`
}

// Exception exempts paths from a check. The reason is mandatory: an exemption
// nobody can explain is indistinguishable from a leak.
type Exception struct {
	Path   string `yaml:"path"`
	Reason string `yaml:"reason"`

	Glob *regexp.Regexp `yaml:"-"`
}

// Hooks configures where graft installs its git hooks.
type Hooks struct {
	Dir string `yaml:"dir"`
}

// Lock configures the per-repository commit lock.
type Lock struct {
	Timeout time.Duration `yaml:"timeout"`
}

// IsEnabled reports whether the secrets check runs; it is on by default.
func (s Secrets) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// IsEnabled reports whether the large file check runs; it is on by default.
func (l LargeFiles) IsEnabled() bool { return l.Enabled == nil || *l.Enabled }

// ErrNotFound is returned when the work tree has no config file.
var ErrNotFound = errors.New(FileName + " not found")

// Load reads and validates the config at the work tree root.
func Load(root string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w in %s: create it (see README)", ErrNotFound, root)
	}
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return cfg, nil
}

// Parse decodes and validates config data. Unknown keys are errors: a typo in
// a key would otherwise silently switch a check off.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Schema != SchemaVersion {
		return fmt.Errorf("schema: must be %d, got %d", SchemaVersion, c.Schema)
	}
	validators := []func() error{
		c.validateVersion,
		c.validateTicket,
		c.validatePush,
		c.validateSecrets,
		c.validateLargeFiles,
		c.validateHooks,
		c.validateLock,
		c.validateEnvs,
		c.validateFlags,
	}
	for _, validate := range validators {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateVersion() error {
	v := &c.Version
	if v.TagPrefix == "" {
		v.TagPrefix = DefaultTagPrefix
	}
	switch v.Mode {
	case ModeFile:
		if v.File == "" {
			v.File = DefaultVersionFile
		}
		if err := checkRelPath("version.file", v.File); err != nil {
			return err
		}
		for i := range v.Sync {
			if err := v.Sync[i].validate(fmt.Sprintf("version.sync[%d]", i)); err != nil {
				return err
			}
		}
		return nil
	case ModeGitTag, ModeNone:
		if v.File != "" || len(v.Sync) > 0 || v.TagOnCommit {
			return fmt.Errorf("version: file, sync and tag_on_commit apply only to mode %q", ModeFile)
		}
		return nil
	case "":
		return fmt.Errorf("version.mode: required, one of %s, %s, %s", ModeFile, ModeGitTag, ModeNone)
	default:
		return fmt.Errorf("version.mode: unknown mode %q, want %s, %s or %s", v.Mode, ModeFile, ModeGitTag, ModeNone)
	}
}

func (s *SyncFile) validate(where string) error {
	if err := checkRelPath(where+".path", s.Path); err != nil {
		return err
	}
	switch s.Format {
	case FormatPlain:
		if len(s.Key) > 0 || s.Pattern != "" {
			return fmt.Errorf("%s: format plain takes neither key nor pattern", where)
		}
	case FormatJSON:
		if len(s.Key) == 0 || s.Pattern != "" {
			return fmt.Errorf("%s: format json needs key (list of object keys) and no pattern", where)
		}
	case FormatRegex:
		if s.Pattern == "" || len(s.Key) > 0 {
			return fmt.Errorf("%s: format regex needs pattern and no key", where)
		}
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return fmt.Errorf("%s.pattern: %w", where, err)
		}
		if re.NumSubexp() != 1 {
			return fmt.Errorf("%s.pattern: needs exactly one capture group around the version, has %d", where, re.NumSubexp())
		}
		s.Regexp = re
	default:
		return fmt.Errorf("%s.format: want %s, %s or %s, got %q", where, FormatPlain, FormatJSON, FormatRegex, s.Format)
	}
	return nil
}

func (c *Config) validateTicket() error {
	if c.Ticket == nil {
		return nil
	}
	if c.Ticket.Pattern == "" {
		return errors.New("ticket.pattern: required when ticket is set")
	}
	re, err := regexp.Compile(c.Ticket.Pattern)
	if err != nil {
		return fmt.Errorf("ticket.pattern: %w", err)
	}
	c.Ticket.Regexp = re
	return nil
}

func (c *Config) validatePush() error {
	seen := map[string]bool{}
	for _, remote := range c.Push.Remotes {
		if remote == "" || seen[remote] {
			return fmt.Errorf("push.remotes: empty or duplicate remote %q", remote)
		}
		seen[remote] = true
	}
	return nil
}

func (c *Config) validateSecrets() error {
	s := &c.Checks.Secrets
	for i, pattern := range s.ExtraPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("checks.secrets.extra_patterns[%d]: %w", i, err)
		}
		s.Extra = append(s.Extra, re)
	}
	return validateExceptions("checks.secrets.exceptions", s.Exceptions)
}

func (c *Config) validateLargeFiles() error {
	l := &c.Checks.LargeFiles
	if l.MaxBinaryBytes == 0 {
		l.MaxBinaryBytes = DefaultMaxBinaryBytes
	}
	if l.MaxTextBytes == 0 {
		l.MaxTextBytes = DefaultMaxTextBytes
	}
	if l.MaxBinaryBytes < 0 || l.MaxTextBytes < 0 {
		return errors.New("checks.large_files: size limits must be positive")
	}
	if l.BinaryExtensions == nil {
		l.BinaryExtensions = slices.Clone(DefaultBinaryExtensions)
	}
	for i, ext := range l.BinaryExtensions {
		if ext == "" || strings.ContainsAny(ext, "./\\") {
			return fmt.Errorf("checks.large_files.binary_extensions[%d]: %q must be a bare extension like png", i, ext)
		}
		l.BinaryExtensions[i] = strings.ToLower(ext)
	}
	return validateExceptions("checks.large_files.exceptions", l.Exceptions)
}

func validateExceptions(where string, exceptions []Exception) error {
	for i := range exceptions {
		e := &exceptions[i]
		if e.Path == "" {
			return fmt.Errorf("%s[%d].path: required", where, i)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return fmt.Errorf("%s[%d] (%s): reason is required", where, i, e.Path)
		}
		glob, err := CompileGlob(e.Path)
		if err != nil {
			return fmt.Errorf("%s[%d].path: %w", where, i, err)
		}
		e.Glob = glob
	}
	return nil
}

func (c *Config) validateHooks() error {
	if c.Hooks.Dir == "" {
		c.Hooks.Dir = DefaultHooksDir
	}
	return checkRelPath("hooks.dir", c.Hooks.Dir)
}

func (c *Config) validateLock() error {
	if c.Lock.Timeout == 0 {
		c.Lock.Timeout = DefaultLockTimeout
	}
	if c.Lock.Timeout < 0 {
		return errors.New("lock.timeout: must be positive")
	}
	return nil
}

// checkRelPath accepts slash-separated paths inside the work tree.
func checkRelPath(where, p string) error {
	if p == "" {
		return fmt.Errorf("%s: required", where)
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("%s: %q must use forward slashes", where, p)
	}
	clean := path.Clean(p)
	if path.IsAbs(p) || filepath.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%s: %q must be a relative path inside the work tree", where, p)
	}
	return nil
}

// Matches reports whether a slash-separated repository path is exempted.
func Matches(exceptions []Exception, p string) bool {
	for _, e := range exceptions {
		if e.Glob.MatchString(p) {
			return true
		}
	}
	return false
}

// CompileGlob turns a gitignore-style glob into an anchored regexp: `*` and
// `?` stay within one path segment, `**` spans segments, and `dir/**/x`
// matches `dir/x` as well.
func CompileGlob(glob string) (*regexp.Regexp, error) {
	if err := checkRelPath("glob", glob); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		ch := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
