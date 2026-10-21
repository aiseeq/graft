package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	Description string `json:"description"`
	// Frozen is set by the caller for a version whose approval has begun:
	// NextVersion never adds items to it.
	Frozen bool `json:"-"`
}

// Unreleased finds the unreleased version of the name.
func Unreleased(versions []Version, name string) (Version, bool) {
	i := slices.IndexFunc(versions, func(v Version) bool { return v.Name == name && !v.Released && !v.Archived })
	if i < 0 {
		return Version{}, false
	}
	return versions[i], true
}

// Approver is one approver of a version and where they stand: PENDING,
// APPROVED or DECLINED.
type Approver struct {
	AccountID string `json:"accountId"`
	Status    string `json:"status"`
}

// ApprovedStatus is the status of an approver who approved the version.
const ApprovedStatus = "APPROVED"

// ApprovalBegun tells whether anybody has approved: from then on the
// approved set of items is the version's and nothing joins it.
func ApprovalBegun(approvers []Approver) bool {
	return slices.ContainsFunc(approvers, func(a Approver) bool { return a.Status == ApprovedStatus })
}

// FullyApproved tells whether the version has approvers and every one of
// them approved.
func FullyApproved(approvers []Approver) bool {
	return len(approvers) > 0 && !slices.ContainsFunc(approvers, func(a Approver) bool { return a.Status != ApprovedStatus })
}

// VersionApprovers reads the approvers of a version.
func (c *Client) VersionApprovers(ctx context.Context, id string) ([]Approver, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/version/"+id+"?expand=approvers", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var raw struct {
		ID        string     `json:"id"`
		Approvers []Approver `json:"approvers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("version %s: unexpected JSON: %w", id, err)
	}
	if raw.ID != id {
		return nil, fmt.Errorf("version %s: the answer is about version %q", id, raw.ID)
	}
	for _, a := range raw.Approvers {
		if a.AccountID == "" || a.Status == "" {
			return nil, fmt.Errorf("version %s: an approver without an account or a status: %+v", id, a)
		}
	}
	return raw.Approvers, nil
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
// released, archived nor frozen and whose names begin with prefix, the one with the
// nearest (earliest) release date, an overdue one included; versions
// without a date come after the dated ones.
// It never guesses: no such version, two sharing the nearest date, or several
// undated ones and no dated one are errors naming the candidates.
func NextVersion(project string, versions []Version, prefix string) (Version, error) {
	var open, frozen, dated, undated []Version
	for _, v := range versions {
		if v.Released || v.Archived {
			continue
		}
		open = append(open, v)
		if !strings.HasPrefix(v.Name, prefix) {
			continue
		}
		if v.Frozen {
			frozen = append(frozen, v)
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
		if len(frozen) > 0 {
			return Version{}, fmt.Errorf("every unreleased version of %s starting with %q is being approved, its items are fixed: %s; add the next version", project, prefix, names(frozen))
		}
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

// DayNames reads the window's day from version names: Date for a name that
// follows the template, LooksLike for a slip of it.
type DayNames interface {
	Date(name string) (time.Time, bool)
	LooksLike(name string) bool
}

// SlipsOf names the unreleased versions whose names look like the template
// but do not follow it: graft never guesses which one is meant.
func SlipsOf(versions []Version, names DayNames) error {
	var slips []string
	for _, v := range versions {
		if !v.Released && !v.Archived && names.LooksLike(v.Name) {
			slips = append(slips, fmt.Sprintf("%q", v.Name))
		}
	}
	if len(slips) > 0 {
		return fmt.Errorf("versions %s look like the version name template but do not follow it: rename them", strings.Join(slips, ", "))
	}
	return nil
}

// NextDatedVersion picks, among the versions that are neither released,
// archived nor frozen and follow the template, the one of the earliest day.
func NextDatedVersion(project string, versions []Version, names DayNames) (Version, error) {
	if err := SlipsOf(versions, names); err != nil {
		return Version{}, fmt.Errorf("%s: %w", project, err)
	}
	var best Version
	var bestDay time.Time
	frozen := 0
	for _, v := range versions {
		day, ok := names.Date(v.Name)
		switch {
		case !ok || v.Released || v.Archived:
		case v.Frozen:
			frozen++
		case best.ID == "" || day.Before(bestDay):
			best, bestDay = v, day
		}
	}
	if best.ID == "" {
		if frozen > 0 {
			return Version{}, fmt.Errorf("every unreleased window version of %s is being approved, its items are fixed; add the next one", project)
		}
		return Version{}, fmt.Errorf("%s has no unreleased window version; the release adds the next one, or add it by hand", project)
	}
	return best, nil
}

// ProjectID reads a project's id, which creating a version takes.
func (c *Client) ProjectID(ctx context.Context, key string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/project/"+key, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var raw struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("project %s: unexpected JSON: %w", key, err)
	}
	if raw.ID == "" {
		return "", fmt.Errorf("project %s: no id in the answer", key)
	}
	return raw.ID, nil
}

// ErrNoVersionRights is the refusal when the token's user may not manage
// versions.
var ErrNoVersionRights = errors.New("managing versions needs the Manage versions permission (MANAGE_VERSIONS); in a team-managed project it comes with the Administrator role of the project")

// RequireVersionRights checks that the token's user may create, edit and
// release the project's versions.
func (c *Client) RequireVersionRights(ctx context.Context, project string) error {
	data, err := c.do(ctx, http.MethodGet, "/rest/api/3/mypermissions?projectKey="+project+"&permissions=MANAGE_VERSIONS", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var raw struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("mypermissions: unexpected JSON: %w", err)
	}
	p, ok := raw.Permissions["MANAGE_VERSIONS"]
	if !ok {
		return errors.New("mypermissions: no answer about MANAGE_VERSIONS")
	}
	if !p.HavePermission {
		return fmt.Errorf("%s: %w", project, ErrNoVersionRights)
	}
	return nil
}

// CreateVersion adds an unreleased version to the project.
func (c *Client) CreateVersion(ctx context.Context, projectID, name, releaseDate string) (Version, error) {
	pid, err := strconv.Atoi(projectID)
	if err != nil {
		return Version{}, fmt.Errorf("project id %q is not a number", projectID)
	}
	body := map[string]any{"projectId": pid, "name": name, "releaseDate": releaseDate, "released": false}
	data, err := c.do(ctx, http.MethodPost, "/rest/api/3/version", body, http.StatusCreated)
	if err != nil {
		return Version{}, err
	}
	var v Version
	if err := json.Unmarshal(data, &v); err != nil {
		return Version{}, fmt.Errorf("version %q: unexpected JSON: %w", name, err)
	}
	if v.ID == "" {
		return Version{}, fmt.Errorf("version %q: no id in the answer", name)
	}
	return v, nil
}

// UpdateVersion changes the given fields of a version: name, description,
// released, releaseDate.
func (c *Client) UpdateVersion(ctx context.Context, id string, fields map[string]any) error {
	_, err := c.do(ctx, http.MethodPut, "/rest/api/3/version/"+id, fields, http.StatusOK)
	return err
}

// RemoveFixVersion takes a version out of an item's fix versions.
func (c *Client) RemoveFixVersion(ctx context.Context, key, versionID string) error {
	body := map[string]any{"update": map[string]any{
		"fixVersions": []any{map[string]any{"remove": map[string]string{"id": versionID}}},
	}}
	_, err := c.do(ctx, http.MethodPut, "/rest/api/3/issue/"+key, body, http.StatusNoContent)
	return err
}

// searchPage is how many items one search request reads.
const searchPage = 100

// VersionItems lists the keys of the items with the version among their
// fix versions.
func (c *Client) VersionItems(ctx context.Context, versionID string) ([]string, error) {
	var keys []string
	token := ""
	for {
		q := url.Values{"jql": {"fixVersion = " + versionID}, "fields": {"key"}, "maxResults": {fmt.Sprint(searchPage)}}
		if token != "" {
			q.Set("nextPageToken", token)
		}
		data, err := c.do(ctx, http.MethodGet, "/rest/api/3/search/jql?"+q.Encode(), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page struct {
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
			NextPageToken string `json:"nextPageToken"`
			IsLast        bool   `json:"isLast"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("version %s items: unexpected JSON: %w", versionID, err)
		}
		for _, i := range page.Issues {
			keys = append(keys, i.Key)
		}
		if page.IsLast || page.NextPageToken == "" {
			slices.Sort(keys)
			return keys, nil
		}
		token = page.NextPageToken
	}
}
