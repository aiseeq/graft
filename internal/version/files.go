package version

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/jsonspan"
)

// Target is a file that holds the version: the version file itself or one of
// the files synchronised with it.
type Target struct {
	// Path is slash-separated and relative to the work tree root.
	Path string
	sync config.SyncFile
}

// Targets lists the version file first, then the synchronised files.
func Targets(v config.Version) []Target {
	targets := []Target{{Path: v.File, sync: config.SyncFile{Path: v.File, Format: config.FormatPlain}}}
	for _, s := range v.Sync {
		targets = append(targets, Target{Path: s.Path, sync: s})
	}
	return targets
}

type span struct{ start, end int }

// Extract returns the version recorded in content. Every occurrence the target
// describes must carry the same value.
func (t Target) Extract(content []byte) (string, error) {
	spans, err := t.locate(content)
	if err != nil {
		return "", err
	}
	value := string(content[spans[0].start:spans[0].end])
	for _, s := range spans[1:] {
		if other := string(content[s.start:s.end]); other != value {
			return "", fmt.Errorf("%s: disagrees with itself: %q and %q", t.Path, value, other)
		}
	}
	return value, nil
}

// Replace returns content with every occurrence of the version set to v.
func (t Target) Replace(content []byte, v string) ([]byte, error) {
	spans, err := t.locate(content)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	prev := 0
	for _, s := range spans {
		out.Write(content[prev:s.start])
		out.WriteString(v)
		prev = s.end
	}
	out.Write(content[prev:])
	return out.Bytes(), nil
}

func (t Target) locate(content []byte) ([]span, error) {
	var (
		spans []span
		err   error
	)
	switch t.sync.Format {
	case config.FormatPlain:
		spans, err = locatePlain(content)
	case config.FormatJSON:
		spans, err = locateJSON(content, t.sync.Key)
	case config.FormatRegex:
		spans, err = locateRegex(content, t.sync)
	default:
		err = fmt.Errorf("unknown format %q", t.sync.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.Path, err)
	}
	return spans, nil
}

func locatePlain(content []byte) ([]span, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, errors.New("file is empty")
	}
	start := bytes.Index(content, trimmed)
	return []span{{start, start + len(trimmed)}}, nil
}

func locateRegex(content []byte, s config.SyncFile) ([]span, error) {
	matches := s.Regexp.FindAllSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("pattern %q matches nothing", s.Pattern)
	}
	spans := make([]span, 0, len(matches))
	for _, m := range matches {
		if m[2] < 0 {
			return nil, fmt.Errorf("pattern %q matched without its capture group", s.Pattern)
		}
		spans = append(spans, span{m[2], m[3]})
	}
	return spans, nil
}

// locateJSON preserves the version target's rule that escaped string values
// are not writable, while jsonspan supplies trustworthy byte offsets.
func locateJSON(content []byte, key []string) ([]span, error) {
	found, err := jsonspan.Locate(content, key)
	if err != nil {
		return nil, err
	}
	out := make([]span, len(found))
	for i, item := range found {
		value := content[item.Start:item.End]
		if bytes.Contains(value, []byte(`\`)) {
			return nil, fmt.Errorf("key %q: escaped string %s is not supported", strings.Join(key, "."), string(content[item.Start-1:item.End+1]))
		}
		out[i] = span{item.Start, item.End}
	}
	return out, nil
}

// Backup holds the original content of files a bump rewrote.
type Backup struct {
	root  string
	files []savedFile
}

type savedFile struct {
	path    string
	content []byte
}

// Paths lists the files the backup covers.
func (b *Backup) Paths() []string {
	paths := make([]string, 0, len(b.files))
	for _, f := range b.files {
		paths = append(paths, f.path)
	}
	return paths
}

// Rebase carries the backup to another base. A file saved as the old base has
// it, as asBase says, takes what next gives. Where the new base has no such
// file, one gone from the work tree leaves the backup, there being nothing to
// put back, and one still there stays as saved, for it holds the bump. Any
// other file stays as saved. On an error the backup stays as it was.
func (b *Backup) Rebase(asBase func(path string, saved []byte) (bool, error), next func(path string) ([]byte, bool, error)) error {
	kept := make([]savedFile, 0, len(b.files))
	for _, f := range b.files {
		same, err := asBase(f.path, f.content)
		if err != nil {
			return err
		}
		if same {
			content, ok, err := next(f.path)
			if err != nil {
				return err
			}
			switch {
			case ok:
				f.content = content
			default:
				_, err := os.Lstat(filepath.Join(b.root, filepath.FromSlash(f.path)))
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return fmt.Errorf("looking at a version target the new HEAD has not: %w", err)
				}
			}
		}
		kept = append(kept, f)
	}
	b.files = kept
	return nil
}

// Restore writes the original content back.
func (b *Backup) Restore() error {
	var errs []error
	for _, f := range b.files {
		if err := writeKeepingMode(filepath.Join(b.root, filepath.FromSlash(f.path)), f.content); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Current reads the version from the version file in the work tree.
func Current(root string, v config.Version) (Semver, error) {
	target := Targets(v)[0]
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(target.Path)))
	if err != nil {
		return Semver{}, fmt.Errorf("reading the version file: %w", err)
	}
	raw, err := target.Extract(content)
	if err != nil {
		return Semver{}, err
	}
	sv, err := ParseFile(raw)
	if err != nil {
		return Semver{}, fmt.Errorf("%s: %w", target.Path, err)
	}
	return sv, nil
}

// Write sets every target in the work tree to next. Nothing is written unless
// every target can be rewritten; a failed write restores the files already
// written.
func Write(root string, v config.Version, next Semver) (*Backup, error) {
	type pending struct {
		path     string
		original []byte
		updated  []byte
	}
	// Several targets may share a file (package-lock.json keeps the version
	// twice): each file is read once, every target's replacement applies to
	// the same buffer in turn, and the file is written once.
	var plan []*pending
	byPath := map[string]*pending{}
	current, currentPath := "", ""
	for i, t := range Targets(v) {
		p, ok := byPath[t.Path]
		if !ok {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.Path)))
			if err != nil {
				return nil, fmt.Errorf("reading a version target: %w", err)
			}
			p = &pending{path: t.Path, original: content, updated: content}
			byPath[t.Path] = p
			plan = append(plan, p)
		}
		// Every sync target holds the version the version file has: one
		// that differs was edited by hand or merged wrong, and the bump
		// would overwrite it unseen.
		has, err := t.Extract(p.original)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			current, currentPath = has, t.Path
		} else if has != current {
			return nil, fmt.Errorf("%s has version %s, %s has %s: make them agree before the bump", t.Path, has, currentPath, current)
		}
		updated, err := t.Replace(p.updated, next.String())
		if err != nil {
			return nil, err
		}
		p.updated = updated
	}
	backup := &Backup{root: root}
	for _, p := range plan {
		if err := writeKeepingMode(filepath.Join(root, filepath.FromSlash(p.path)), p.updated); err != nil {
			if restoreErr := backup.Restore(); restoreErr != nil {
				return nil, errors.Join(err, fmt.Errorf("restoring version files: %w", restoreErr))
			}
			return nil, err
		}
		backup.files = append(backup.files, savedFile{p.path, p.original})
	}
	return backup, nil
}

// writeKeepingMode rewrites an existing file, lifting and restoring a
// read-only bit: generated files are often read-only to discourage hand edits,
// and the bump is the one writer that must get through.
func writeKeepingMode(path string, content []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("rewriting a version target: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode|0o200); err != nil {
			return fmt.Errorf("making a version target writable: %w", err)
		}
	}
	writeErr := os.WriteFile(path, content, mode)
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode); err != nil {
			return errors.Join(writeErr, err)
		}
	}
	return writeErr
}
