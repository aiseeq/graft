package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Version is a release of a Jira project, a value of an item's fix versions.
type Version struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Released bool   `json:"released"`
	Archived bool   `json:"archived"`
	// ReleaseDate is YYYY-MM-DD, empty when the version has none.
	ReleaseDate string `json:"releaseDate"`
}

// ProjectVersions lists every version of a project.
func (c *Client) ProjectVersions(ctx context.Context, project string) ([]Version, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/project/"+project+"/versions", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var versions []Version
	if err := json.Unmarshal(data, &versions); err != nil {
		return nil, fmt.Errorf("%s: unexpected versions JSON: %w", project, err)
	}
	for _, v := range versions {
		if v.ID == "" || v.Name == "" {
			return nil, fmt.Errorf("%s: a version without an id or a name: %+v", project, v)
		}
	}
	return versions, nil
}

// AddFixVersion adds a version to an item's fix versions and leaves the
// others it has alone. Editing the item is all the permission it takes.
func (c *Client) AddFixVersion(ctx context.Context, key, versionID string) error {
	body := map[string]any{"update": map[string]any{
		"fixVersions": []any{map[string]any{"add": map[string]string{"id": versionID}}},
	}}
	_, err := c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+key, body, http.StatusNoContent)
	return err
}

// NextVersion picks, among the versions of a project that are neither
// released nor archived and whose names begin with prefix, the one with the
// nearest (earliest) release date, an overdue one included; versions
// without a date come after the dated ones.
// It never guesses: no such version, two sharing the nearest date, or several
// undated ones and no dated one are errors naming the candidates.
func NextVersion(project string, versions []Version, prefix string) (Version, error) {
	var open, dated, undated []Version
	for _, v := range versions {
		if v.Released || v.Archived {
			continue
		}
		open = append(open, v)
		if !strings.HasPrefix(v.Name, prefix) {
			continue
		}
		if v.ReleaseDate == "" {
			undated = append(undated, v)
			continue
		}
		if _, err := time.Parse(time.DateOnly, v.ReleaseDate); err != nil {
			return Version{}, fmt.Errorf("%s: version %q has release date %q, not YYYY-MM-DD", project, v.Name, v.ReleaseDate)
		}
		dated = append(dated, v)
	}
	if len(dated) > 0 {
		// ISO dates sort as strings.
		slices.SortStableFunc(dated, func(a, b Version) int { return strings.Compare(a.ReleaseDate, b.ReleaseDate) })
		nearest := dated[:1]
		for _, v := range dated[1:] {
			if v.ReleaseDate == dated[0].ReleaseDate {
				nearest = append(nearest, v)
			}
		}
		if len(nearest) > 1 {
			return Version{}, fmt.Errorf("unreleased versions of %s starting with %q share the nearest release date %s: %s; release, archive or move one of them",
				project, prefix, dated[0].ReleaseDate, names(nearest))
		}
		return dated[0], nil
	}
	switch len(undated) {
	case 1:
		return undated[0], nil
	case 0:
		if len(open) == 0 {
			return Version{}, fmt.Errorf("no unreleased version of %s starts with %q: %s has no unreleased versions", project, prefix, project)
		}
		return Version{}, fmt.Errorf("no unreleased version of %s starts with %q (unreleased: %s)", project, prefix, names(open))
	default:
		return Version{}, fmt.Errorf("unreleased versions of %s starting with %q: %s, none has a release date; give the next one a date", project, prefix, names(undated))
	}
}

func names(versions []Version) string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Name
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}
