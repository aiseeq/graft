package checks

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
)

// Finding is one problem in the staged changes.
type Finding struct {
	Path   string
	Line   int // 0 when the finding concerns the whole file
	Reason string
	// Leak marks a private name or term headed for a public repository.
	Leak bool
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", f.Path, f.Line, f.Reason)
	}
	return fmt.Sprintf("%s: %s", f.Path, f.Reason)
}

type secretPattern struct {
	name string
	re   *regexp.Regexp
}

// builtinSecrets are credential formats with a recognisable shape. Quoted
// forms are required where the bare prefix is common in ordinary text.
var builtinSecrets = []secretPattern{
	{"PEM private key", regexp.MustCompile(`-----BEGIN (RSA |OPENSSH |EC |DSA )?PRIVATE KEY-----`)},
	{"API key (re_...)", regexp.MustCompile(`['"]re_[A-Za-z0-9_-]{20,}['"]`)},
	{"OAuth client secret (GOCSPX-...)", regexp.MustCompile(`['"]GOCSPX-[A-Za-z0-9_-]{20,}['"]`)},
	{"PGPASSWORD assignment", regexp.MustCompile(`PGPASSWORD=[A-Za-z0-9_./+=-]{20,}`)},
	{"AWS access key id", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"live secret key (sk_live_...)", regexp.MustCompile(`['"]sk_live_[A-Za-z0-9]{20,}['"]`)},
	{"JWT literal", regexp.MustCompile(`['"]eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}['"]`)},
}

// Secrets scans staged text files for credentials. Findings name the file,
// line and kind of secret but never echo the secret itself: the output ends up
// in terminals, logs and chat transcripts.
func Secrets(files []StagedFile, cfg config.Secrets) []Finding {
	patterns := slices.Clone(builtinSecrets)
	for _, re := range cfg.Extra {
		patterns = append(patterns, secretPattern{"extra pattern " + re.String(), re})
	}
	var findings []Finding
	for _, f := range files {
		if config.Matches(cfg.Exceptions, f.Path) {
			continue
		}
		if f.Content == nil {
			findings = append(findings, Finding{Path: f.Path, Reason: fmt.Sprintf("too large to scan for secrets (%d bytes)", f.Size)})
			continue
		}
		if f.IsBinary() {
			continue
		}
		for i, line := range bytes.Split(f.Content, []byte("\n")) {
			for _, p := range patterns {
				if p.re.Match(line) {
					findings = append(findings, Finding{Path: f.Path, Line: i + 1, Reason: "possible " + p.name})
				}
			}
		}
	}
	return findings
}

// LargeFiles rejects binaries that are not an expected asset type, files
// over the size limits and symlinks to absolute paths. Binary content does
// not diff and stays in the history forever, so its limit is stricter. An
// absolute symlink leads into this machine, nowhere in another clone: one
// made by mistake inside the tree is staged by a commit that stages all.
func LargeFiles(files []StagedFile, cfg config.LargeFiles) []Finding {
	var findings []Finding
	for _, f := range files {
		if config.Matches(cfg.Exceptions, f.Path) {
			continue
		}
		if f.Symlink {
			if target := string(f.Content); isAbs(target) {
				findings = append(findings, Finding{Path: f.Path, Reason: fmt.Sprintf("symlink to the absolute path %s, which leads nowhere in another clone", target)})
			}
			continue
		}
		binary := f.IsBinary()
		ext := strings.TrimPrefix(strings.ToLower(path.Ext(f.Path)), ".")
		if binary && !slices.Contains(cfg.BinaryExtensions, ext) {
			findings = append(findings, Finding{Path: f.Path, Reason: fmt.Sprintf("binary file (%s); build outputs belong in .gitignore", formatSize(f.Size))})
			continue
		}
		limit := cfg.MaxTextBytes
		if binary {
			limit = cfg.MaxBinaryBytes
		}
		if f.Size > limit {
			findings = append(findings, Finding{Path: f.Path, Reason: fmt.Sprintf("%s exceeds the limit of %s", formatSize(f.Size), formatSize(limit))})
		}
	}
	return findings
}

// isAbs tells whether a symlink target is absolute on any system, /x, \x
// or C:x, whichever system made the link.
func isAbs(target string) bool {
	drive := len(target) >= 2 && target[1] == ':' &&
		('a' <= target[0] && target[0] <= 'z' || 'A' <= target[0] && target[0] <= 'Z')
	return drive || strings.HasPrefix(target, "/") || strings.HasPrefix(target, `\`)
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
