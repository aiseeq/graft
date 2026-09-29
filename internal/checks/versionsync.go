package checks

import (
	"fmt"

	"github.com/aiseeq/graft/internal/config"
	"github.com/aiseeq/graft/internal/gitx"
	"github.com/aiseeq/graft/internal/version"
)

// maxVersionFileBytes bounds the version files read from the index.
const maxVersionFileBytes = 16 << 20

// VersionSync verifies, in the index, that the version file holds a valid
// version and every synchronised file repeats it. A hand-resolved merge
// conflict is the usual way they drift apart.
func VersionSync(repo *gitx.Repo, v config.Version) ([]Finding, error) {
	if v.Mode != config.ModeFile {
		return []Finding{}, nil
	}
	targets := version.Targets(v)
	specs := make([]string, len(targets))
	for i, t := range targets {
		specs[i] = ":" + t.Path
	}
	blobs, err := repo.ReadBlobs(specs, maxVersionFileBytes)
	if err != nil {
		return nil, fmt.Errorf("reading staged version files: %w", err)
	}
	main := targets[0]
	want, err := main.Extract(blobs[0].Content)
	if err == nil {
		_, err = version.Parse(want)
	}
	if err != nil {
		return []Finding{{Path: main.Path, Reason: err.Error()}}, nil
	}
	findings := []Finding{}
	for i, t := range targets[1:] {
		got, err := t.Extract(blobs[i+1].Content)
		switch {
		case err != nil:
			findings = append(findings, Finding{Path: t.Path, Reason: err.Error()})
		case got != want:
			findings = append(findings, Finding{Path: t.Path, Reason: fmt.Sprintf("version %q, %s says %q", got, main.Path, want)})
		}
	}
	return findings, nil
}
