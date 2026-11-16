package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/envs"
	"github.com/aiseeq/graft/v2/internal/jira"
)

type reviewUnderWay struct {
	key, status, account, name, version string
}

func reviewerAgreement(value string) map[string]bool {
	got := map[string]bool{}
	for token := range strings.SplitSeq(value, ",") {
		if strings.TrimSpace(token) != "" {
			got[strings.TrimSpace(token)] = true
		}
	}
	return got
}

// reviewGuard checks the Jira version currently running at a guarded target,
// and matching keys found in commits delivered to that target.
func (a *App) reviewGuard(ctx context.Context, r *deployRun) ([]reviewUnderWay, error) {
	if r.target.ReviewGuard == nil {
		return nil, nil
	}
	client, err := jiraClient()
	if err != nil {
		return nil, fmt.Errorf("review guard: Jira unavailable: %w", err)
	}
	versionLabel, issues, err := reviewGuardScope(ctx, r, client)
	if err != nil {
		return nil, err
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return nil, fmt.Errorf("review guard: identify Jira user: %w", err)
	}
	return reviewGuardIssues(issues, r.target.ReviewGuard.Statuses, me, versionLabel)
}

func reviewGuardScope(ctx context.Context, r *deployRun, client *jira.Client) (string, []*jira.Issue, error) {
	if r.target.DeployedSHA != nil {
		current, err := r.readSHA(ctx, r.env, r.target.DeployedSHA)
		if err != nil {
			return "", nil, fmt.Errorf("review guard: read target deployed_sha: %w", err)
		}
		r.prev = current
	}
	targetVersion, err := reviewGuardTargetVersion(ctx, r)
	if err != nil {
		return "", nil, err
	}
	if targetVersion == "" {
		return "", nil, errors.New("review guard: unknown target release: target version is unavailable")
	}
	versionName := strings.ReplaceAll(r.target.ReviewGuard.JiraVersion, "{version}", targetVersion)
	versions, available, err := reviewGuardVersions(ctx, r, client, versionName)
	if err != nil {
		return "", nil, err
	}
	if len(versions) == 0 {
		return "", nil, fmt.Errorf("review guard: unknown target release: computed Jira version %q was not found; nearby unreleased versions: %s", versionName, strings.Join(available, ", "))
	}
	commitKeys, err := reviewGuardCommitKeys(ctx, r)
	if err != nil {
		return "", nil, fmt.Errorf("review guard: read deployed commit keys: %w", err)
	}
	commitIssues, err := reviewGuardReadCommitIssues(ctx, client, commitKeys)
	if err != nil {
		return "", nil, err
	}
	versionIDs := make([]string, len(versions))
	for i, version := range versions {
		versionIDs[i] = version.ID
	}
	versionLabel := versionName
	commitParents := make([]string, 0, len(commitIssues))
	for key, issue := range commitIssues {
		if len(issue.FixVersions) == 0 || len(issue.FixVersionIDs(versionIDs)) > 0 {
			commitParents = append(commitParents, key)
		}
	}
	allIssues, err := reviewGuardVersionIssues(ctx, client, versions, r.target.ReviewGuard.Statuses, commitParents)
	if err != nil {
		return "", nil, err
	}
	for key, issue := range commitIssues {
		if len(issue.FixVersions) == 0 || len(issue.FixVersionIDs(versionIDs)) > 0 {
			if _, alreadyFromVersion := allIssues[key]; !alreadyFromVersion {
				allIssues[key] = issue
			}
		}
	}
	result := make([]*jira.Issue, 0, len(allIssues))
	for _, issue := range allIssues {
		result = append(result, issue)
	}
	slices.SortFunc(result, func(a, b *jira.Issue) int { return strings.Compare(a.Key, b.Key) })
	return versionLabel, result, nil
}

func reviewGuardVersionIssues(ctx context.Context, client *jira.Client, versions []jira.Version, statuses, extraParents []string) (map[string]*jira.Issue, error) {
	all := map[string]*jira.Issue{}
	statusJQL := reviewGuardJQLStatuses(statuses)
	parents := slices.Clone(extraParents)
	for _, version := range versions {
		items, err := client.ReviewSearchIssues(ctx, "fixVersion = "+version.ID+" AND status in ("+statusJQL+")")
		if err != nil {
			return nil, fmt.Errorf("review guard: read Jira version %s (%s): %w", version.Name, version.ID, err)
		}
		for i := range items {
			issue := &items[i]
			all[issue.Key] = issue
		}
		versionParents, err := client.ReviewSearchIssueKeys(ctx, "fixVersion = "+version.ID)
		if err != nil {
			return nil, fmt.Errorf("review guard: read parent keys for Jira version %s: %w", version.Name, err)
		}
		parents = append(parents, versionParents...)
	}
	for _, batch := range reviewGuardBatches(uniqueSorted(parents), 100) {
		jql := "parent in (" + strings.Join(batch, ",") + ") AND status in (" + statusJQL + ")"
		subtasks, err := client.ReviewSearchIssues(ctx, jql)
		if err != nil {
			return nil, fmt.Errorf("review guard: read subtasks of Jira version parents: %w", err)
		}
		for i := range subtasks {
			issue := &subtasks[i]
			all[issue.Key] = issue
		}
	}
	return all, nil
}

func reviewGuardTargetVersion(ctx context.Context, r *deployRun) (string, error) {
	state, err := readDeployed(ctx, r.repo, r.cfg, r.env, r.target, r.prev)
	if err != nil {
		return "", fmt.Errorf("review guard: cannot identify target version: %w", err)
	}
	if state.failed || state.version == "no version command" || state.version == "" {
		return "", fmt.Errorf("unknown target version: %s", state.version)
	}
	return strings.TrimSuffix(state.version, " by its commit"), nil
}

func reviewGuardReadCommitIssues(ctx context.Context, client *jira.Client, keys []string) (map[string]*jira.Issue, error) {
	issues := make(map[string]*jira.Issue, len(keys))
	for _, batch := range reviewGuardBatches(keys, 100) {
		jql := "key in (" + strings.Join(batch, ",") + ")"
		items, err := client.ReviewSearchIssues(ctx, jql)
		if err != nil {
			return nil, fmt.Errorf("review guard: read delivered Jira issues: %w", err)
		}
		found := make(map[string]bool, len(items))
		for i := range items {
			issue := &items[i]
			issues[issue.Key] = issue
			found[issue.Key] = true
		}
		for _, key := range batch {
			if !found[key] {
				return nil, fmt.Errorf("review guard: Jira search did not return delivered issue %s", key)
			}
		}
	}
	return issues, nil
}

func reviewGuardVersions(ctx context.Context, r *deployRun, client *jira.Client, name string) ([]jira.Version, []string, error) {
	var selected []jira.Version
	var open []jira.Version
	for _, project := range r.cfg.ReleaseNotes.Jira.ProjectKeys {
		projectVersions, err := client.ProjectVersions(ctx, project)
		if err != nil {
			return nil, nil, fmt.Errorf("review guard: read Jira versions for %s: %w", project, err)
		}
		for _, version := range projectVersions {
			if !version.Released && !version.Archived {
				open = append(open, version)
				if version.Name == name {
					selected = append(selected, version)
				}
			}
		}
	}
	slices.SortFunc(selected, func(a, b jira.Version) int { return strings.Compare(a.ID, b.ID) })
	return selected, nearestReviewVersions(name, open, 5), nil
}

func reviewGuardIssues(issues []*jira.Issue, statuses []string, me, versionName string) ([]reviewUnderWay, error) {
	var active []reviewUnderWay
	for _, issue := range issues {
		if issue.AssigneeID == "" || issue.AssigneeID == me || !slices.Contains(statuses, issue.Status) {
			continue
		}
		name := issue.AssigneeName
		if name == "" {
			name = issue.AssigneeID
		}
		active = append(active, reviewUnderWay{key: issue.Key, status: issue.Status, account: issue.AssigneeID, name: name, version: versionName})
	}
	return active, nil
}

func reviewGuardJQLStatuses(statuses []string) string {
	quoted := make([]string, len(statuses))
	for i, status := range statuses {
		quoted[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(status, `\`, `\\`), `"`, `\"`) + `"`
	}
	return strings.Join(quoted, ",")
}

func reviewGuardBatches(items []string, size int) [][]string {
	var batches [][]string
	for len(items) > 0 {
		n := min(size, len(items))
		batches = append(batches, items[:n])
		items = items[n:]
	}
	return batches
}

func nearestReviewVersions(name string, versions []jira.Version, limit int) []string {
	byName := map[string]int{}
	for _, version := range versions {
		distance := reviewLevenshtein(name, version.Name)
		if old, ok := byName[version.Name]; !ok || distance < old {
			byName[version.Name] = distance
		}
	}
	names := slices.Sorted(maps.Keys(byName))
	slices.SortFunc(names, func(a, b string) int {
		if byName[a] != byName[b] {
			return byName[a] - byName[b]
		}
		return strings.Compare(a, b)
	})
	return names[:min(limit, len(names))]
}

func reviewLevenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ar := range a {
		cur := make([]int, len(b)+1)
		cur[0] = i + 1
		for j, br := range b {
			cost := 0
			if ar != br {
				cost = 1
			}
			cur[j+1] = min(cur[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func reviewGuardCommitKeys(ctx context.Context, r *deployRun) ([]string, error) {
	if r.prev == "" {
		return nil, nil
	}
	base := ""
	if r.target.Requires != "" {
		req := r.cfg.Deploy.Targets[r.target.Requires]
		if req != nil && req.DeployedSHA != nil {
			env, err := deployEnv(r, req.Env)
			if err != nil {
				return nil, err
			}
			base, err = r.readSHA(ctx, env, req.DeployedSHA)
			if err != nil {
				return nil, err
			}
		}
	}
	if base == "" && r.cfg.Deploy.Targets["prod"] != nil && r.cfg.Deploy.Targets["prod"].DeployedSHA != nil {
		prod := r.cfg.Deploy.Targets["prod"]
		env, err := deployEnv(r, prod.Env)
		if err != nil {
			return nil, err
		}
		base, err = r.readSHA(ctx, env, prod.DeployedSHA)
		if err != nil {
			return nil, err
		}
	}
	pattern := projectKeyRe(r.cfg)
	var output string
	var err error
	if base != "" {
		output, err = r.repo.Git("log", "--format=%B", base+".."+r.prev)
		if err != nil {
			return nil, err
		}
	} else {
		// With no reference target, inspect ancestors newest first and retain
		// at most the first 500 commits that contain a project key.
		out, e := r.repo.Git("rev-list", r.prev)
		if e != nil {
			return nil, e
		}
		commits := strings.Fields(out)
		var keys []string
		keyedCommits := 0
		for _, sha := range commits {
			body, e := r.repo.Git("show", "-s", "--format=%B", sha)
			if e != nil {
				return nil, e
			}
			found := pattern.FindAllString(body, -1)
			if len(found) == 0 {
				continue
			}
			keyedCommits++
			keys = append(keys, found...)
			if keyedCommits >= 500 {
				break
			}
		}
		return uniqueSorted(keys), nil
	}
	return uniqueSorted(pattern.FindAllString(output, -1)), nil
}

func deployEnv(r *deployRun, name string) (*envs.Env, error) {
	return envs.Get(r.cfg, r.repo.Root, name)
}

func (a *App) enforceReviewGuard(ctx context.Context, r *deployRun, agreement string, reportOnly bool) error {
	if r.target.ReviewGuard == nil {
		return nil
	}
	active, err := a.reviewGuard(ctx, r)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	if reportOnly {
		r.reviewGuardActive = true
		a.printReviewGuard(r.name, active)
		return nil
	}
	accepted := reviewerAgreement(agreement)
	var missing []string
	var required []string
	for _, item := range active {
		pair := item.key + "@" + item.account
		required = append(required, pair)
		if !accepted[pair] {
			missing = append(missing, pair)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var details []string
	for _, item := range active {
		details = append(details, fmt.Sprintf("  %s: %s; reviewer %s (%s)", item.key, item.status, item.name, item.account))
	}
	return fmt.Errorf("review guard: %s Jira version %s has work under review:\n%s\na response from each reviewer is needed; after the response run graft deploy %s --reviewer-agreed %s", r.name, active[0].version, strings.Join(details, "\n"), r.name, strings.Join(required, ","))
}

func (a *App) printReviewGuard(target string, active []reviewUnderWay) {
	a.printf("%s Jira version %s has work under review:", target, active[0].version)
	for _, item := range active {
		a.printf("- %s: %s; reviewer %s (%s); agreement token %s@%s", item.key, item.status, item.name, item.account, item.key, item.account)
	}
}
