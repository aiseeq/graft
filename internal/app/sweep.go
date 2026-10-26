package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
	"github.com/aiseeq/graft/v2/internal/jira"
)

// sweepEvery is how often graft commit sweeps a target; sweepTimeout bounds
// that sweep, a reminder beside the commit and not part of it.
const (
	sweepEvery   = 24 * time.Hour
	sweepTimeout = 3 * time.Minute
	// sweepReads is how many items the sweep reads from Jira at once.
	sweepReads = 8
	// sweepMargin widens the walk that tells where a commit is, so a commit
	// of the window behind an older merge is still found.
	sweepMargin = 30 * 24 * time.Hour
)

// DeploySweep is graft deploy sweep <target>: it reads the commit the target
// runs, as graft deploy check does, and reconciles the items with it; apply
// moves them, otherwise it only lists what it would move.
func (a *App) DeploySweep(ctx context.Context, target string, apply bool) error {
	r, err := a.deployTarget(target)
	if err != nil {
		return err
	}
	if r.target.ReleaseNotes == nil || r.target.ReleaseNotes.Sweep == nil {
		return fmt.Errorf("deploy.targets.%s.release_notes.sweep is not configured", target)
	}
	r.dryRun = true
	head, withheld, err := a.sweepHead(ctx, r)
	if err != nil {
		return err
	}
	return a.sweep(ctx, r, head, withheld, apply)
}

// sweepHead reads the commit the target runs and the commits withheld from
// it, and records both for the daily sweep; when the target cannot be read
// without a password, the record of the last deploy from here stands in.
func (a *App) sweepHead(ctx context.Context, r *deployRun) (string, []string, error) {
	head, err := r.readSHA(ctx, r.env, r.target.DeployedSHA)
	var withheld []string
	if err == nil && head != "" && r.target.ApprovedRelease != nil {
		withheld, err = r.readWithheld(ctx)
	}
	if err == nil && head != "" {
		if err := writeSweepRecord(r.repo, r.name, head, withheld); err != nil {
			a.warn("%v", err)
		}
		return head, withheld, nil
	}
	recHead, recWithheld, recErr := readSweepRecord(r.repo, r.name)
	switch {
	case recErr != nil:
		return "", nil, recErr
	case recHead == "" && err != nil:
		return "", nil, err
	case recHead == "":
		return "", nil, fmt.Errorf("%s runs no recorded commit yet", r.name)
	}
	if err != nil {
		a.warn("%v; the commit the last deploy from here recorded, %s, stands in", err, gitx.Short(recHead))
	}
	return recHead, recWithheld, nil
}

// deploySweep sweeps the target a deploy has just updated and records the
// commit it runs for the daily sweep, which then waits a day.
func (a *App) deploySweep(ctx context.Context, r *deployRun) {
	conf := r.target.ReleaseNotes.Sweep
	if conf == nil {
		return
	}
	if err := writeSweepRecord(r.repo, r.name, r.head, r.withheld); err != nil {
		a.warn("%v", err)
	}
	if err := a.sweep(ctx, r, r.head, r.withheld, conf.Apply); err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	if stamp, err := sweepPath(r.repo, "sweep", r.name); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	} else if err := touch(stamp); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	}
}

// dailySweep sweeps, once a day, each target with release_notes.sweep that a
// deploy from this repository recorded, after a commit: the items the
// release notes of single deploys left behind still get their move.
func (a *App) dailySweep(ctx context.Context, repo *gitx.Repo, cfg *config.Config) {
	if cfg.Deploy == nil {
		return
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range cfg.Deploy.Targets {
			if !yield(n) {
				return
			}
		}
	}) {
		t := cfg.Deploy.Targets[name]
		if t.ReleaseNotes == nil || t.ReleaseNotes.Sweep == nil {
			continue
		}
		a.dailySweepTarget(ctx, &deployRun{repo: repo, cfg: cfg, name: name, target: t})
	}
}

func (a *App) dailySweepTarget(ctx context.Context, r *deployRun) {
	stamp, err := sweepPath(r.repo, "sweep", r.name)
	if err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < sweepEvery {
		return
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	head, withheld, err := readSweepRecord(r.repo, r.name)
	if err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	if head == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	if err := a.sweep(ctx, r, head, withheld, r.target.ReleaseNotes.Sweep.Apply); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	}
	if err := touch(stamp); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	}
}

// sweepPath is a file of the sweep of a target, kept in the user cache per
// repository.
func sweepPath(repo *gitx.Repo, kind, target string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache: %w", err)
	}
	sum := sha256.Sum256([]byte(repo.CommonDir + "\x00" + target))
	return filepath.Join(cache, "graft", kind, hex.EncodeToString(sum[:12])), nil
}

// writeSweepRecord keeps the commit a target runs and those withheld from
// it, for the daily sweep, which reads no server.
func writeSweepRecord(repo *gitx.Repo, target, head string, withheld []string) error {
	path, err := sweepPath(repo, "deployed", target)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(path, []byte(strings.Join(append([]string{head}, withheld...), "\n")+"\n"), 0o600)
	}
	if err != nil {
		return fmt.Errorf("the commit %s runs is not recorded for the daily sweep: %w", target, err)
	}
	return nil
}

// readSweepRecord reads what writeSweepRecord kept; head is "" without a
// record.
func readSweepRecord(repo *gitx.Repo, target string) (head string, withheld []string, err error) {
	path, err := sweepPath(repo, "deployed", target)
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading the commit %s runs: %w", target, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", nil, nil
	}
	return fields[0], fields[1:], nil
}

// sweptCommit is one commit of an item: whether the target runs it and
// whether the trunk has it.
type sweptCommit struct {
	sha               string
	when              time.Time
	onTarget, inTrunk bool
}

// sweepRun reconciles the items of one target.
type sweepRun struct {
	app    *App
	r      *deployRun
	conf   *config.Sweep
	notes  *config.JiraNotes
	client *jira.Client
	gate   *approvalGate
	me     string
	trunk  string
	now    time.Time
	apply  bool
	issues map[string]jira.Issue
	// lines are the items to look at by hand; moved are the items moved, or
	// that would be.
	lines []string
	moved map[string]bool
}

// sweep moves to the done status the items whose work the target runs in
// full, and lists those whose status git contradicts. A failure is the
// caller's to report: the deploy and the commit go on.
func (a *App) sweep(ctx context.Context, r *deployRun, head string, withheld []string, apply bool) error {
	known, err := r.repo.RevExists(head)
	if err != nil {
		return err
	}
	if !known {
		return fmt.Errorf("%s runs %s, which is unknown here (git fetch?)", r.name, gitx.Short(head))
	}
	remotes, err := r.repo.Remotes()
	if err != nil {
		return err
	}
	trunk, err := trunkRef(r.repo, r.cfg, "", remotes)
	if err != nil {
		return err
	}
	now, err := deployNow()
	if err != nil {
		return err
	}
	conf := r.target.ReleaseNotes.Sweep
	items, err := sweepItems(r, head, trunk, withheld, now.AddDate(0, 0, -conf.Days))
	if err != nil {
		return err
	}
	how := "moves"
	if !apply {
		how = "lists only (release_notes.sweep.apply is off; graft deploy sweep " + r.name + " --apply moves)"
	}
	a.printf("sweep %s: %d items in the commits of the last %d days on %s and %s; it %s", r.name, len(items), conf.Days, gitx.Short(head), trunk, how)
	if len(items) == 0 {
		return nil
	}
	client, err := jiraClient()
	if err != nil {
		return err
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return fmt.Errorf("whose items are whose is unknown: %w", err)
	}
	s := &sweepRun{app: a, r: r, conf: conf, notes: r.cfg.ReleaseNotes.Jira, client: client, me: me, trunk: trunk, now: now, apply: apply,
		issues: map[string]jira.Issue{}, moved: map[string]bool{}}
	if ar := r.target.ApprovedRelease; ar != nil {
		s.gate = &approvalGate{client: client, conf: ar, issues: s.issues, versions: map[string]bool{}}
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range items {
			if !yield(k) {
				return
			}
		}
	})
	s.read(ctx, keys)
	s.parents(ctx, keys, items)
	for _, key := range keys {
		if issue, ok := s.issues[key]; ok && !s.moved[key] && !slices.Contains(s.notes.SkipKeys, key) {
			s.item(ctx, issue, items[key])
		}
	}
	if len(s.lines) > 0 {
		a.printf("sweep %s: to look at by hand:\n  %s", r.name, strings.Join(s.lines, "\n  "))
	} else if len(s.moved) == 0 {
		a.printf("sweep %s: nothing to move or look at", r.name)
	}
	return nil
}

// sweepItems groups the commits of the window, on any remote branch or on
// the target, merges left out, by the item each is the work of. A revert and
// the commits it reverts are left out: together they deliver nothing.
// Commits withheld from the target are not on it, though its history has
// them.
func sweepItems(r *deployRun, head, trunk string, withheld []string, since time.Time) (map[string][]sweptCommit, error) {
	out, err := r.repo.Git("log", "--no-merges", "--format=%H%x1f%ct%x1f%s%x1f%b%x1e", "--since="+since.Format(time.RFC3339), "--remotes", head)
	if err != nil {
		return nil, err
	}
	type logged struct {
		deliveredCommit
		when time.Time
	}
	var commits []logged
	for rec := range strings.SplitSeq(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected commit time %q of %s", f[1], f[0])
		}
		commits = append(commits, logged{deliveredCommit{sha: f[0], subject: f[2], body: f[3]}, time.Unix(sec, 0)})
	}
	onTarget, err := revSet(r.repo, head, since.Add(-sweepMargin))
	if err != nil {
		return nil, err
	}
	inTrunk, err := revSet(r.repo, trunk, since.Add(-sweepMargin))
	if err != nil {
		return nil, err
	}
	reverted := map[string]bool{}
	for _, c := range commits {
		if !isRevert(c.subject) {
			continue
		}
		for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
			if full, err := r.repo.Git("rev-parse", "--verify", "--quiet", m[1]+"^{commit}"); err == nil {
				reverted[strings.TrimSpace(full)] = true
			}
		}
	}
	keyRe := projectKeyRe(r.cfg)
	items := map[string][]sweptCommit{}
	for _, c := range commits {
		if isRevert(c.subject) || reverted[c.sha] {
			continue
		}
		key := workKey(keyRe, r.target.ApprovedRelease, c.deliveredCommit)
		if key == "" {
			continue
		}
		items[key] = append(items[key], sweptCommit{sha: c.sha, when: c.when, onTarget: onTarget[c.sha] && !slices.Contains(withheld, c.sha), inTrunk: inTrunk[c.sha]})
	}
	return items, nil
}

// revSet is the set of commits rev reaches, back to since.
func revSet(repo *gitx.Repo, rev string, since time.Time) (map[string]bool, error) {
	out, err := repo.Git("rev-list", "--since="+since.Format(time.RFC3339), rev)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for sha := range strings.FieldsSeq(out) {
		set[sha] = true
	}
	return set, nil
}

// workKey is the item a commit is the work of: an attribution of the
// target's approved_release, or the first key of the subject, or else a line
// of the body that is a key alone, as graft commit adds the key of the
// branch. A key the body only mentions ("after PROJ-12") refers to another
// item; "" is a commit of no item.
func workKey(keyRe *regexp.Regexp, conf *config.ApprovedRelease, c deliveredCommit) string {
	if conf != nil {
		if at, ok := conf.Attributed[c.sha]; ok {
			return at.Item
		}
	}
	if key := keyRe.FindString(c.subject); key != "" {
		return key
	}
	for line := range strings.Lines(c.body) {
		if line = strings.TrimSpace(line); line != "" && keyRe.FindString(line) == line {
			return line
		}
	}
	return ""
}

// read reads the items from Jira a few at a time; an item that fails is
// reported and left out.
func (s *sweepRun) read(ctx context.Context, keys []string) {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, sweepReads)
	)
	for _, key := range keys {
		if _, ok := s.issues[key]; ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			issue, err := s.client.Issue(ctx, key)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.app.warn("sweep %s: %s not read: %v", s.r.name, key, err)
				return
			}
			s.issues[key] = issue
		}()
	}
	wg.Wait()
}

// item judges one item by its commits.
func (s *sweepRun) item(ctx context.Context, issue jira.Issue, commits []sweptCommit) {
	allOn := !slices.ContainsFunc(commits, func(c sweptCommit) bool { return !c.onTarget })
	inTrunk := slices.ContainsFunc(commits, func(c sweptCommit) bool { return c.inTrunk })
	key, status := issue.Key, issue.Status
	switch {
	case issue.Done:
		if i := slices.IndexFunc(commits, func(c sweptCommit) bool { return !c.inTrunk }); i >= 0 {
			s.list(ctx, issue, "%s is %s, but %s is not in %s", key, status, gitx.Short(commits[i].sha), s.trunk)
		} else if i := slices.IndexFunc(commits, func(c sweptCommit) bool { return !c.onTarget }); i >= 0 {
			s.list(ctx, issue, "%s is %s, but %s is not on %s yet", key, status, gitx.Short(commits[i].sha), s.r.name)
		}
	case issue.NotStarted:
		if inTrunk {
			s.list(ctx, issue, "%s is %s, but its commits are in %s", key, status, s.trunk)
		}
	case s.finishing(status):
		if allOn {
			s.move(ctx, issue, fmt.Sprintf("its work of the last %d days is on %s (commits: %d)", s.conf.Days, s.r.name, len(commits)), true)
		}
	case s.inProgress(status):
		newest := slices.MaxFunc(commits, func(a, b sweptCommit) int { return a.when.Compare(b.when) })
		if days := int(s.now.Sub(newest.when).Hours() / 24); allOn && days >= s.conf.StaleDays {
			s.list(ctx, issue, "%s is %s, but all its work is on %s, the newest commit %d days old", key, status, s.r.name, days)
		}
	case !s.inFlow(status):
		if inTrunk {
			s.list(ctx, issue, "%s is %s, but its commits are in %s", key, status, s.trunk)
		}
	}
}

// parents moves to the done status a parent all of whose subtasks are done
// and whose work, its own and its subtasks', the target runs in full: the
// commits name the subtasks, so no deploy moves the parent.
func (s *sweepRun) parents(ctx context.Context, keys []string, items map[string][]sweptCommit) {
	var parents []string
	for _, key := range keys {
		if issue, ok := s.issues[key]; ok {
			if issue.Parent != "" {
				parents = append(parents, issue.Parent)
			} else if len(issue.Subtasks) > 0 {
				parents = append(parents, key)
			}
		}
	}
	parents = uniqueSorted(parents)
	s.read(ctx, parents)
	allOn := func(key string) bool {
		return !slices.ContainsFunc(items[key], func(c sweptCommit) bool { return !c.onTarget })
	}
	for _, key := range parents {
		parent, ok := s.issues[key]
		if !ok || parent.Done || len(parent.Subtasks) == 0 || slices.Contains(s.notes.SkipKeys, key) || !allOn(key) {
			continue
		}
		s.read(ctx, parent.Subtasks)
		done := true
		for _, st := range parent.Subtasks {
			issue, ok := s.issues[st]
			done = done && ok && issue.Done && allOn(st)
		}
		if done {
			s.move(ctx, parent, fmt.Sprintf("its %d subtasks are done and their work is on %s", len(parent.Subtasks), s.r.name), false)
		}
	}
}

// finishing tells whether the target's transition to the done status moves
// an item in status.
func (s *sweepRun) finishing(status string) bool {
	t := s.r.target.ReleaseNotes.Moving(status)
	return t != nil && t.To == s.conf.Done
}

// inProgress tells whether a transition of some target moves an item in
// status elsewhere than to the done status: In Progress to Testing, say.
func (s *sweepRun) inProgress(status string) bool {
	for _, t := range s.r.cfg.Deploy.Targets {
		if n := t.ReleaseNotes; n != nil {
			if m := n.Moving(status); m != nil && m.To != s.conf.Done {
				return true
			}
		}
	}
	return false
}

// inFlow tells whether some target's transitions or approved_release name
// status; an item elsewhere, Paused or Blocked, say, stays there by hand.
func (s *sweepRun) inFlow(status string) bool {
	for _, t := range s.r.cfg.Deploy.Targets {
		if n := t.ReleaseNotes; n != nil && (n.Moving(status) != nil || n.Leading(status) != nil) {
			return true
		}
		if ar := t.ApprovedRelease; ar != nil && slices.Contains(ar.Statuses, status) {
			return true
		}
	}
	return false
}

// move takes an item to the done status when it is the token owner's and,
// with approved_release, passes it: by statuses and version for an item
// itself (checked), by version alone for a parent the sweep finishes.
func (s *sweepRun) move(ctx context.Context, issue jira.Issue, why string, checked bool) {
	key := issue.Key
	if slices.Contains(s.notes.SkipStatuses, issue.Status) {
		return
	}
	if !s.owned(ctx, issue) {
		return
	}
	if s.gate != nil {
		pass, err := s.gatePass(ctx, issue, checked)
		if err != nil {
			s.app.warn("sweep %s: %s not moved: %v", s.r.name, key, err)
			return
		}
		if !pass.ok {
			s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: %s", key, issue.Status, why, pass.why))
			return
		}
	}
	s.moved[key] = true
	if !s.apply {
		s.app.printf("sweep %s: %s would move to %s: %s", s.r.name, key, s.conf.Done, why)
		return
	}
	if err := s.client.Transition(ctx, issue, s.conf.Done); err != nil {
		s.app.warn("sweep %s: %s not moved to %s: %v", s.r.name, key, s.conf.Done, err)
		return
	}
	s.app.printf("sweep %s: %s moved to %s: %s", s.r.name, key, s.conf.Done, why)
}

// gatePass checks an item against approved_release: a parent the sweep
// finishes is not in its statuses yet, so only its version counts.
func (s *sweepRun) gatePass(ctx context.Context, issue jira.Issue, checked bool) (gatePass, error) {
	if checked {
		return s.gate.item(ctx, issue.Key)
	}
	if s.gate.conf.Prefix == "" {
		return gatePass{ok: true}, nil
	}
	version, err := s.gate.approvedVersion(ctx, issue)
	if err != nil || version != "" {
		return gatePass{ok: version != ""}, err
	}
	return gatePass{why: fmt.Sprintf("%s not in an approved %q version", issue.Key, s.gate.conf.Prefix)}, nil
}

// list adds a line about an item of the token owner's.
func (s *sweepRun) list(ctx context.Context, issue jira.Issue, format string, args ...any) {
	if s.owned(ctx, issue) {
		s.lines = append(s.lines, fmt.Sprintf(format, args...))
	}
}

// owned tells whether the item is the token owner's now or was once: a key
// in a commit does not make an item yours.
func (s *sweepRun) owned(ctx context.Context, issue jira.Issue) bool {
	if issue.AssigneeID == s.me {
		return true
	}
	yours, err := s.client.WasAssignedTo(ctx, issue.Key, s.me)
	if err != nil {
		s.app.warn("sweep %s: whose %s is is unknown: %v", s.r.name, issue.Key, err)
	}
	return yours
}
