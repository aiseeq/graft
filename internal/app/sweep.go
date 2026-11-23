package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aiseeq/graft/v2/internal/atomicfile"
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
	return a.sweep(ctx, r, func(ctx context.Context) (string, []string, error) { return a.sweepHead(ctx, r) }, apply, true)
}

// sweepHead reads the commit the target runs and the commits withheld from
// it, and records both for the daily sweep; when the target cannot be read
// without a password, the record of the last deploy from here stands in.
// Only a run holding the target's lock of its moves records: one outside it
// could write what it read over the newer record of a deploy.
func (a *App) sweepHead(ctx context.Context, r *deployRun) (string, []string, error) {
	head, err := r.readSHA(ctx, r.env, r.target.DeployedSHA)
	var withheld []string
	if err == nil && head != "" && r.target.ApprovedRelease != nil {
		withheld, err = r.readWithheld(ctx)
	}
	if err == nil && head != "" {
		if r.movesLocked {
			if err := writeSweepRecord(r.repo, r.name, head, withheld); err != nil {
				a.warn("%v", err)
			}
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
	if err := a.sweep(ctx, r, func(context.Context) (string, []string, error) { return r.head, r.withheld, nil }, conf.Apply, false); err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	if stamp, err := sweepPath(r.repo, "sweep", r.name); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	} else if err := writeSweepStamp(stamp, conf.Apply); err != nil {
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
		a.dailySweepTarget(ctx, &deployRun{a: a, repo: repo, cfg: cfg, name: name, target: t})
	}
}

func (a *App) dailySweepTarget(ctx context.Context, r *deployRun) {
	stamp, err := sweepPath(r.repo, "sweep", r.name)
	if err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	}
	if fresh, err := sweepStampFresh(stamp, r.target.ReleaseNotes.Sweep.Apply); err != nil {
		a.warn("sweep %s: %v; sweeping anyway", r.name, err)
	} else if fresh {
		return
	}
	if head, _, err := readSweepRecord(r.repo, r.name); err != nil {
		a.warn("sweep %s: %v", r.name, err)
		return
	} else if head == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	// What the target runs is read again under the lock: a deploy the sweep
	// waited for has recorded anew.
	snapshot := func(context.Context) (string, []string, error) {
		head, withheld, err := readSweepRecord(r.repo, r.name)
		if err == nil && head == "" {
			err = fmt.Errorf("%s runs no recorded commit", r.name)
		}
		return head, withheld, err
	}
	if err := a.sweep(ctx, r, snapshot, r.target.ReleaseNotes.Sweep.Apply, false); err != nil {
		// Not marked swept: the next commit sweeps again.
		a.warn("sweep %s: %v; the daily sweep is not done, the next commit tries it again", r.name, err)
		return
	}
	if err := writeSweepStamp(stamp, r.target.ReleaseNotes.Sweep.Apply); err != nil {
		a.warn("sweep %s: %v", r.name, err)
	}
}

// sweepStampFresh tells whether the daily sweep in this apply mode ran less
// than a day ago. A stamp of the other mode, an old format or a stamp that
// cannot be read is not fresh: the sweep runs and writes it anew.
func sweepStampFresh(path string, apply bool) (bool, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading sweep stamp: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading sweep stamp: %w", err)
	}
	mode, err := strconv.ParseBool(strings.TrimSpace(string(data)))
	return err == nil && mode == apply && time.Since(fi.ModTime()) < sweepEvery, nil
}

func writeSweepStamp(path string, apply bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating sweep stamp directory: %w", err)
	}
	if err := atomicfile.Write(path, []byte(strconv.FormatBool(apply)+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing sweep stamp: %w", err)
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
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
		err = atomicfile.Write(path, []byte(strings.Join(append([]string{head}, withheld...), "\n")+"\n"), 0o600)
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
// whether the trunk has it; shipsNothing, on the target only as it changes
// nothing there but approved_release.ignore_paths.
type sweptCommit struct {
	sha                             string
	when                            time.Time
	onTarget, inTrunk, shipsNothing bool
}

// onTargetWords says that the commits, all of them, are on the target, and
// which of them only as they ship nothing to it.
func onTargetWords(target string, commits []sweptCommit) string {
	idle := 0
	for _, c := range commits {
		if c.shipsNothing {
			idle++
		}
	}
	switch idle {
	case 0:
		return "is on " + target
	case len(commits):
		return "ships nothing to " + target + ", changing only approved_release.ignore_paths"
	}
	return fmt.Sprintf("is on %s, but for %d commits that ship nothing to it, changing only approved_release.ignore_paths", target, idle)
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
	// head is the commit the target runs; since, where the window begins.
	head   string
	trunk  string
	now    time.Time
	since  time.Time
	apply  bool
	issues map[string]jira.Issue
	items  map[string][]sweptCommit
	// mover moves the items as a deploy would; tried are the items given
	// to it, parentMoves those of them that are parents moved for their
	// subtasks' work, a held one to be judged again before its handover.
	mover       *noteMover
	tried       map[string]bool
	parentMoves map[string]bool
	// win is the window the items come from.
	win *sweepWindow
	// review are the statuses work waits in for a reviewer.
	review []string
	// lines are the items to look at by hand; judged are the items moved,
	// or that would be, and the parents listed: their own commits say no
	// more.
	lines  []string
	judged map[string]bool
	// glint holds the moves for the glint findings of the commit the
	// target runs, as it holds the transitions'.
	glint *glintCheck
}

// sweep moves to the done status the items whose work the target runs in
// full, and lists those whose status git contradicts. A failure is the
// caller's to report: the deploy and the commit go on. A manual sweep, graft
// deploy sweep, moves by its --apply alone. One that moves holds the
// target's lock of its moves from reading what the target runs, by
// snapshot, to recording what stays held: a deploy it waited for has
// changed that.
func (a *App) sweep(ctx context.Context, r *deployRun, snapshot func(context.Context) (string, []string, error), apply, manual bool) error {
	if !apply {
		// A sweep that only lists moves nothing and records nothing held.
		return a.sweepOnce(ctx, r, snapshot, apply, manual)
	}
	return a.withMoves(ctx, r, func() error { return a.sweepOnce(ctx, r, snapshot, apply, manual) })
}

// sweepOnce is the sweep of the commit the target runs by snapshot.
func (a *App) sweepOnce(ctx context.Context, r *deployRun, snapshot func(context.Context) (string, []string, error), apply, manual bool) error {
	head, withheld, err := snapshot(ctx)
	if err != nil {
		return err
	}
	s, err := a.sweepPass(r, head, withheld, apply)
	if err != nil {
		return err
	}
	conf := s.conf
	how := "moves"
	switch {
	case apply:
	case manual && conf.Apply:
		how = "lists only: pass --apply to move (release_notes.sweep.apply: true moves after deploys and daily)"
	case manual:
		how = "lists only: pass --apply to move"
	default:
		how = "lists only (release_notes.sweep.apply is off; graft deploy sweep " + r.name + " --apply moves)"
	}
	a.printf("sweep %s: %d items in the commits of the last %d days on %s and %s; it %s", r.name, len(s.items), conf.Days, gitx.Short(head), s.trunk, how)
	if len(s.items) == 0 && len(s.win.reverted) == 0 {
		return nil
	}
	if err := s.connect(ctx); err != nil {
		return err
	}
	s.judge(ctx, s.items)
	return s.keepHeld()
}

// sweepWindow is what git says of the window of a target running head: the
// commits by item, the items a revert took work out of, and, read on the
// first parent that asks, the items with work older than the window or only
// on a local branch. One run reads it once: a deploy's release notes and its
// sweep share it.
type sweepWindow struct {
	key        string
	trunk      string
	now, since time.Time
	items      map[string][]sweptCommit
	reverted   map[string]bool
	read       bool
	older      map[string]bool
	local      map[string]bool
}

// sweepPass groups, from git alone, the commits of the window of the target
// running head by item.
func (a *App) sweepPass(r *deployRun, head string, withheld []string, apply bool) (*sweepRun, error) {
	conf := r.target.ReleaseNotes.Sweep
	key := head + "\x00" + strings.Join(withheld, " ") + "\x00" + strconv.Itoa(conf.Days)
	if r.sweepWindow == nil || r.sweepWindow.key != key {
		w, err := readSweepWindow(r, head, withheld)
		if err != nil {
			return nil, err
		}
		w.key = key
		r.sweepWindow = w
	}
	w := r.sweepWindow
	return &sweepRun{app: a, r: r, conf: conf, notes: r.cfg.ReleaseNotes.Jira, head: head, trunk: w.trunk, now: w.now, since: w.since, apply: apply,
		issues: map[string]jira.Issue{}, items: w.items, win: w, judged: map[string]bool{}, glint: r.glint}, nil
}

func readSweepWindow(r *deployRun, head string, withheld []string) (*sweepWindow, error) {
	known, err := r.repo.RevExists(head)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, fmt.Errorf("%s runs %s, which is unknown here (git fetch?)", r.name, gitx.Short(head))
	}
	remotes, err := r.repo.Remotes()
	if err != nil {
		return nil, err
	}
	trunk, err := trunkRef(r.repo, r.cfg, "", remotes)
	if err != nil {
		return nil, err
	}
	now, err := deployNow()
	if err != nil {
		return nil, err
	}
	since := now.AddDate(0, 0, -r.target.ReleaseNotes.Sweep.Days)
	items, reverted, err := sweepItems(r, head, trunk, withheld, since)
	if err != nil {
		return nil, err
	}
	return &sweepWindow{trunk: trunk, now: now, since: since, items: items, reverted: reverted}, nil
}

// connect reads who owns the Jira token and readies the checks of the moves.
func (s *sweepRun) connect(ctx context.Context) error {
	client, err := jiraClient()
	if err != nil {
		return err
	}
	me, err := client.Myself(ctx)
	if err != nil {
		return fmt.Errorf("whose items are whose is unknown: %w", err)
	}
	s.client, s.me = client, me
	s.review = s.r.cfg.ReviewStatuses()
	if s.glint == nil {
		head := s.head
		if s.glint, err = newGlintCheck(s.r, "", func(context.Context) (string, error) { return head, nil }); err != nil {
			return err
		}
	}
	if ar := s.r.target.ApprovedRelease; ar != nil {
		s.gate = newApprovalGate(client, s.r.cfg, s.r.target, s.issues)
	}
	return nil
}

// noteMover is the mover of the release notes, by which the sweep moves an
// item the way a deploy would: with the transition's assign and
// require_comment.
func (s *sweepRun) noteMover() *noteMover {
	if s.mover == nil {
		var standing []string
		if s.notes != nil {
			standing = s.notes.SkipKeys
		}
		s.mover = &noteMover{app: s.app, client: s.client, conf: s.r.target.ReleaseNotes, approved: s.r.target.ApprovedRelease, me: s.me, target: s.r.name, glint: s.glint, standing: standing}
	}
	return s.mover
}

// keepHeld records the items the pass held for their handover beside those
// held before: an item the pass tried to move again loses its old line. A
// record not written fails the pass: no handover would find what it held.
func (s *sweepRun) keepHeld() error {
	if len(s.tried) == 0 {
		return nil
	}
	held, err := readHeld(s.r)
	if err != nil {
		return err
	}
	held = slices.DeleteFunc(held, func(h heldItem) bool { return h.after == "" && s.tried[h.key] })
	for _, h := range s.mover.held {
		h.parent = s.parentMoves[h.key]
		held = append(held, h)
	}
	return writeHeld(s.r, held)
}

// judge moves and lists the items, then prints what is to look at.
func (s *sweepRun) judge(ctx context.Context, items map[string][]sweptCommit) {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range items {
			if !yield(k) {
				return
			}
		}
	})
	// A revert leaves no commit of its item, yet its parent is listed: what
	// of its work the target runs is unknown.
	withReverted := keys
	if s.win != nil {
		withReverted = uniqueSorted(append(slices.Collect(maps.Keys(s.win.reverted)), keys...))
	}
	s.read(ctx, withReverted)
	s.parents(ctx, withReverted)
	for _, key := range keys {
		if issue, ok := s.issues[key]; ok && !s.judged[key] && !isSkipKey(s.notes.SkipKeys, key) {
			if item, dormant := s.r.target.ApprovedRelease.DormantFor(key, issue.Parent); dormant {
				if s.judged[item.Key] {
					s.judged[key] = true
					continue
				}
				s.judged[key] = true
				s.lines = append(s.lines, dormantLeftMessage(item, issue))
				continue
			}
			s.item(ctx, issue, items[key])
		}
	}
	if len(s.lines) > 0 {
		s.app.printf("sweep %s: to look at by hand:\n  %s", s.r.name, strings.Join(s.lines, "\n  "))
	} else if len(s.judged) == 0 {
		s.app.printf("sweep %s: nothing to move or look at", s.r.name)
	}
}

// revertedIn names the commits the reverts among commits revert; a name that
// resolves to no commit counts for nothing.
func revertedIn(r *deployRun, commits []loggedCommit) (map[string]bool, error) {
	reverted := map[string]bool{}
	plain := make([]deliveredCommit, len(commits))
	for i := range commits {
		plain[i] = commits[i].deliveredCommit
	}
	removing, err := workRemovingReverts(r.repo, plain)
	if err != nil {
		return nil, err
	}
	for _, c := range commits {
		if !removing[c.sha] {
			continue
		}
		for _, m := range revertsRe.FindAllStringSubmatch(c.body, -1) {
			if full, err := r.repo.Git("rev-parse", "--verify", "--quiet", m[1]+"^{commit}"); err == nil {
				reverted[strings.TrimSpace(full)] = true
			}
		}
	}
	return reverted, nil
}

// sweepItems groups the commits of the window, on any remote branch or on
// the target, merges left out, by the item each is the work of. A revert and
// the commits it reverts are left out: together they deliver nothing;
// reverted names the items of both, whose work a revert took out. Commits
// withheld from the target are not on it, though its history has them,
// whatever their content.
func sweepItems(r *deployRun, head, trunk string, withheld []string, since time.Time) (map[string][]sweptCommit, map[string]bool, error) {
	commits, err := sweepLog(r, since, "--remotes", head)
	if err != nil {
		return nil, nil, err
	}
	onTarget, inTrunk, sameHeadTrunk, err := sweepCommitSets(r, head, trunk, since)
	if err != nil {
		return nil, nil, err
	}
	reverted, err := revertedIn(r, commits)
	if err != nil {
		return nil, nil, err
	}
	picks, err := pickedOnto(r, commits, onTarget, withheld)
	if err != nil {
		return nil, nil, err
	}
	keyRe := projectKeyRe(r.cfg)
	items := map[string][]sweptCommit{}
	revertedKeys := map[string]bool{}
	plain := make([]deliveredCommit, len(commits))
	for i := range commits {
		plain[i] = commits[i].deliveredCommit
	}
	removing, err := workRemovingReverts(r.repo, plain)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range commits {
		keys := workKeys(keyRe, r.target.ApprovedRelease, c.deliveredCommit)
		if removing[c.sha] || reverted[c.sha] {
			for _, key := range keys {
				revertedKeys[key] = true
			}
			continue
		}
		if len(keys) == 0 {
			continue
		}
		held := slices.Contains(withheld, c.sha) && !picks.onTarget[c.sha]
		sc := sweptCommit{sha: c.sha, when: c.when, onTarget: onTarget[c.sha] && !held || picks.onTarget[c.sha], inTrunk: inTrunk[c.sha]}
		// A pick is in the trunk when what it took is.
		if took := picks.took[c.sha]; len(took) > 0 && !sc.inTrunk {
			sc.inTrunk = !slices.ContainsFunc(took, func(sha string) bool { return !inTrunk[sha] })
		}
		if err := sc.byContent(r, head, trunk, c.parent, sameHeadTrunk, held); err != nil {
			return nil, nil, err
		}
		for _, key := range keys {
			items[key] = append(items[key], sc)
		}
	}
	return items, revertedKeys, nil
}

func sweepCommitSets(r *deployRun, head, trunk string, since time.Time) (map[string]bool, map[string]bool, bool, error) {
	onTarget, err := revSet(r.repo, head, since.Add(-sweepMargin))
	if err != nil {
		return nil, nil, false, err
	}
	inTrunk, err := revSet(r.repo, trunk, since.Add(-sweepMargin))
	if err != nil {
		return nil, nil, false, err
	}
	sameHeadTrunk, err := sameCommit(r, head, trunk)
	if err != nil {
		return nil, nil, false, err
	}
	return onTarget, inTrunk, sameHeadTrunk, nil
}

func sameCommit(r *deployRun, a, b string) (bool, error) {
	refs, err := r.repo.Git("rev-parse", a+"^{commit}", b+"^{commit}")
	if err != nil {
		return false, err
	}
	resolved := strings.Fields(refs)
	return len(resolved) == 2 && resolved[0] == resolved[1], nil
}

// pickedRe is the line git cherry-pick -x adds; a commit that takes another
// in a shape of its own can name it the same way.
var pickedRe = regexp.MustCompile(`(?m)^\(cherry picked from commit ([0-9a-f]{7,40})\)\s*$`)

// picks are the commits the target runs as picks: onTarget the commits
// picked, took what each pick on the target names.
type picks struct {
	onTarget map[string]bool
	took     map[string][]string
}

// pickedOnto reads the picks among the commits the target runs; a name
// that resolves to no commit counts for nothing.
func pickedOnto(r *deployRun, commits []loggedCommit, onTarget map[string]bool, withheld []string) (picks, error) {
	p := picks{onTarget: map[string]bool{}, took: map[string][]string{}}
	for _, c := range commits {
		if !onTarget[c.sha] || slices.Contains(withheld, c.sha) {
			continue
		}
		for _, m := range pickedRe.FindAllStringSubmatch(c.body, -1) {
			full, err := r.repo.Git("rev-parse", "--verify", "--quiet", m[1]+"^{commit}")
			if gitx.ExitCode(err) == 1 {
				continue
			}
			if err != nil {
				return p, err
			}
			sha := strings.TrimSpace(full)
			p.onTarget[sha] = true
			p.took[c.sha] = append(p.took[c.sha], sha)
		}
	}
	return p, nil
}

// loggedCommit is a commit of the sweep's window.
type loggedCommit struct {
	deliveredCommit
	when time.Time
	// parent is its parent, "" for a root commit.
	parent string
}

// sweepLog lists the commits of the window on any remote branch or on head,
// merges left out.
func sweepLog(r *deployRun, since time.Time, revs ...string) ([]loggedCommit, error) {
	args := []string{"log", "--no-merges", "--format=%H%x1f%ct%x1f%P%x1f%s%x1f%b%x1e"}
	if !since.IsZero() {
		args = append(args, "--since="+since.Format(time.RFC3339))
	}
	args = append(args, revs...)
	out, err := r.repo.Git(args...)
	if err != nil {
		return nil, err
	}
	var commits []loggedCommit
	for rec := range strings.SplitSeq(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 5)
		if len(f) != 5 {
			return nil, fmt.Errorf("unexpected git log record %q", rec)
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected commit time %q of %s", f[1], f[0])
		}
		commits = append(commits, loggedCommit{deliveredCommit{sha: f[0], subject: f[3], body: f[4]}, time.Unix(sec, 0), f[2]})
	}
	return commits, nil
}

// byContent completes where the commit is by content: one a release branch
// took runs on the target, and is in the trunk, under another hash.
func (sc *sweptCommit) byContent(r *deployRun, head, trunk, parent string, sameHeadTrunk, withheld bool) error {
	if parent == "" {
		return nil
	}
	var err error
	if withheld {
		// The target does not run a commit withheld from it, whatever it
		// has by content: only whether the trunk has it is asked.
		if !sc.inTrunk {
			sc.inTrunk, err = landed(r, trunk, sc.sha, parent, nil)
		}
		return err
	}
	if sc.onTarget && !sc.inTrunk {
		if sc.inTrunk, err = landed(r, trunk, sc.sha, parent, nil); err != nil {
			return err
		}
	}
	if !sc.onTarget && !sc.inTrunk {
		if sc.inTrunk, err = landed(r, trunk, sc.sha, parent, nil); err != nil || !sc.inTrunk {
			return err
		}
	}
	if sameHeadTrunk && sc.inTrunk {
		sc.onTarget = true
	}
	if sc.inTrunk && !sc.onTarget {
		if sc.onTarget, err = landed(r, head, sc.sha, parent, nil); err != nil || sc.onTarget || r.target.ApprovedRelease == nil {
			return err
		}
		if sc.onTarget, err = landed(r, head, sc.sha, parent, r.target.ApprovedRelease); err != nil || !sc.onTarget {
			return err
		}
		// What the target lacks is ignored files alone; the commit ships
		// nothing only when it changes nothing else.
		sc.shipsNothing, err = changesOnlyIgnored(r, sc.sha, parent, r.target.ApprovedRelease)
	}
	return err
}

// changesOnlyIgnored tells whether commit sha changes, against parent, no
// file but the version files and the files conf ignores.
func changesOnlyIgnored(r *deployRun, sha, parent string, conf *config.ApprovedRelease) (bool, error) {
	out, err := r.repo.Git("diff-tree", "-r", "-z", "--name-only", "--no-renames", parent, sha)
	if err != nil {
		return false, err
	}
	files, ignored := classifyShipChanges(strings.Split(out, "\x00"), r.cfg.VersionFiles(), conf)
	return len(files) == 0 && len(ignored) > 0, nil
}

// landed tells whether rev has the change of commit sha by content: put onto
// rev, the commit changes no file but the version files and, with conf, the
// files it ignores; a conflict counts as a change. A cherry-pick, two
// commits taken in one, a commit of docs alone land; a change rev has
// changed further since conflicts, and does not.
func landed(r *deployRun, rev, sha, parent string, conf *config.ApprovedRelease) (bool, error) {
	out, err := r.repo.Git("merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", "--merge-base="+parent, rev, sha)
	if err != nil && gitx.ExitCode(err) != 1 {
		return false, err
	}
	// The tree, then the files in conflict.
	tree, conflicts, _ := strings.Cut(out, "\x00")
	diff, err := r.repo.Git("diff", "-z", "--name-only", "--no-renames", rev, tree)
	if err != nil {
		return false, err
	}
	files, _ := classifyShipChanges(slices.Concat(strings.Split(conflicts, "\x00"), strings.Split(diff, "\x00")), r.cfg.VersionFiles(), conf)
	return len(files) == 0, nil
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

// Between the keys of one commit's subject: a key right after a work key by
// these alone, or opening a clause or a sentence, is work too; a key anywhere
// else refers to another item ("PROJ-1 fix after PROJ-2").
var (
	workKeysGapRe   = regexp.MustCompile(`^[\s,+/&]*(?:(?:и|and)[\s,+/&]*)?$`)
	workClauseGapRe = regexp.MustCompile(`;\s*$|\.\s+$`)
	// A dot after a short form or in an ellipsis opens no sentence.
	workShortGapRe = regexp.MustCompile(`(?i)(?:^|[\s(])(?:см|ср|напр|e\.g|i\.e|incl|cf|vs)\.\s+$|\.\.\s+$`)
)

// workKeys are the items a commit is the work of, in the order it names them:
// an attribution of the target's approved_release, or the first key of the
// subject with the keys workKeysGapRe and workClauseGapRe let in, or else the
// lines of the body that are a key alone, as graft commit adds the key of the
// branch. A key the body only mentions ("after PROJ-12") refers to another
// item; none is a commit of no item.
func workKeys(keyRe *regexp.Regexp, conf *config.ApprovedRelease, c deliveredCommit) []string {
	if items := attributedItems(conf, c); len(items) > 0 {
		return items
	}
	return messageWorkKeys(keyRe, c)
}

// messageWorkKeys are the items the message itself says a commit is the work
// of, as workKeys tells them.
func messageWorkKeys(keyRe *regexp.Regexp, c deliveredCommit) []string {
	var keys []string
	prevEnd, afterWork := 0, false
	for i, at := range keyRe.FindAllStringIndex(c.subject, -1) {
		gap := c.subject[prevEnd:at[0]]
		afterWork = i == 0 || afterWork && workKeysGapRe.MatchString(gap) || workClauseGapRe.MatchString(gap) && !workShortGapRe.MatchString(gap)
		if afterWork {
			keys = append(keys, c.subject[at[0]:at[1]])
		}
		prevEnd = at[1]
	}
	if len(keys) == 0 {
		for line := range strings.Lines(c.body) {
			if line = strings.TrimSpace(line); line != "" && keyRe.FindString(line) == line {
				keys = append(keys, line)
			}
		}
	}
	seen := map[string]bool{}
	return slices.DeleteFunc(keys, func(k string) bool {
		dup := seen[k]
		seen[k] = true
		return dup
	})
}

// read reads the items from Jira a few at a time; an item that fails is
// reported and left out.
func (s *sweepRun) read(ctx context.Context, keys []string) {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, sweepReads)
	)
	var missing []string
	for _, key := range uniqueSorted(keys) {
		if _, ok := s.issues[key]; !ok {
			missing = append(missing, key)
		}
	}
	for _, key := range missing {
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
	case issue.NotStarted || slices.Contains(s.notes.SkipStatuses, status):
		// Not started, or put aside by hand: work in the trunk says otherwise.
		if inTrunk {
			s.list(ctx, issue, "%s is %s, but its commits are in %s", key, status, s.trunk)
		}
	case s.finishing(status):
		// An item whose type the transition does not move is told by move.
		s.finishingItem(ctx, issue, commits, allOn)
	default:
		// In work, whether a transition names the status or not.
		newest := slices.MaxFunc(commits, func(a, b sweptCommit) int { return a.when.Compare(b.when) })
		if days := int(s.now.Sub(newest.when).Hours() / 24); allOn && days >= s.conf.StaleDays {
			s.list(ctx, issue, "%s is %s, but all its work %s, the newest commit %d days old", key, status, onTargetWords(s.r.name, commits), days)
		}
	}
}

// finishingItem moves work the target runs in full and lists a partial
// finishing item whose remaining commits are only on non-trunk branches.
func (s *sweepRun) finishingItem(ctx context.Context, issue jira.Issue, commits []sweptCommit, allOn bool) {
	if allOn {
		s.move(ctx, issue, fmt.Sprintf("its work of the last %d days %s (commits: %d)", s.conf.Days, onTargetWords(s.r.name, commits), len(commits)), true, false)
		return
	}
	s.listStaleFinishing(ctx, issue, commits)
}

// listStaleFinishing names a remote branch holding the first commit of a
// finishing item that has some work on the target and none of its other work
// in the trunk.
func (s *sweepRun) listStaleFinishing(ctx context.Context, issue jira.Issue, commits []sweptCommit) {
	onTarget, missingOffTrunk := 0, 0
	for _, c := range commits {
		if c.onTarget {
			onTarget++
		} else if !c.inTrunk {
			missingOffTrunk++
		}
	}
	if onTarget == 0 || missingOffTrunk != len(commits)-onTarget {
		return
	}

	i := slices.IndexFunc(commits, func(c sweptCommit) bool { return !c.onTarget })
	branch := ""
	branches, err := s.r.repo.RemoteBranchesContaining(commits[i].sha)
	if err != nil {
		s.app.warn("sweep %s: checking remote branches containing %s: %v", s.r.name, gitx.Short(commits[i].sha), err)
	} else {
		names := make([]string, 0, len(branches))
		for _, b := range branches {
			names = append(names, b.Remote+"/"+b.Branch)
		}
		slices.Sort(names)
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("%d more", len(names)-3))
		}
		if len(names) > 0 {
			branch = " is on " + strings.Join(names, ", ")
		}
	}
	s.list(ctx, issue, "%s is %s, but %d of its %d commits are neither on %s nor in the trunk: %s%s; a branch left from before a rebase is deleted, other work is deployed or brought in",
		issue.Key, issue.Status, len(commits)-onTarget, len(commits), s.r.name, gitx.Short(commits[i].sha), branch)
}

// parents moves on a parent all of whose subtasks are done and whose work,
// its own and its subtasks', the target runs in full: the commits name the
// subtasks, so no deploy moves the parent. It goes by the target's
// transition from its status, as a deploy would move it, or else to the
// done status. A parent in a status outside the flow, or not started, is
// listed instead.
func (s *sweepRun) parents(ctx context.Context, keys []string) {
	parents := s.parentKeys(keys)
	s.read(ctx, parents)
	for _, key := range parents {
		parent, ok := s.issues[key]
		if !ok || isSkipKey(s.notes.SkipKeys, key) {
			continue
		}
		if !parent.Done && len(parent.Subtasks) > 0 {
			s.finishParent(ctx, parent)
		}
	}
}

// parentKeys are the parents among the items and of them, sorted.
func (s *sweepRun) parentKeys(keys []string) []string {
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
	return uniqueSorted(parents)
}

// finishParent moves the parent when its subtasks are all done and on the
// target.
func (s *sweepRun) finishParent(ctx context.Context, parent jira.Issue) {
	stay := func(line string) {
		s.judged[parent.Key] = true
		s.lines = append(s.lines, line)
	}
	if line := s.parentStays(ctx, parent); line != "" {
		stay(line)
		return
	}
	why := fmt.Sprintf("its subtasks are all done (%d) and their work is on %s", len(parent.Subtasks), s.r.name)
	if parent.NotStarted || !s.inFlow(parent) {
		// A parent put aside by hand, Paused say, stays there.
		stay(fmt.Sprintf("%s is %s, but %s", parent.Key, parent.Status, why))
		return
	}
	t, unknown := s.parentRoute(parent)
	if unknown != "" {
		stay(fmt.Sprintf("%s stays %s though %s: %s", parent.Key, parent.Status, why, unknown))
		return
	}
	if t == nil {
		s.judged[parent.Key] = true
		return
	}
	owned, err := s.assignment(ctx, parent)
	if err != nil {
		s.app.warn("sweep %s: whose %s is is unknown: %v", s.r.name, parent.Key, err)
		stay(fmt.Sprintf("%s stays %s: assignment history could not be read: %v", parent.Key, parent.Status, err))
		return
	}
	if !owned {
		stay(fmt.Sprintf("%s stays %s: you are neither its current nor a previous executor", parent.Key, parent.Status))
		return
	}
	if s.parentMoves == nil {
		s.parentMoves = map[string]bool{}
	}
	s.parentMoves[parent.Key] = true
	s.take(ctx, parent, why, false, true, t)
}

// parentStays says why the parent stays where it is, "" when all its
// subtasks are done by Jira and all the work, its own and theirs, runs on
// the target. Where the sweep cannot tell, the parent stays: a subtask
// without a commit of the window, work a revert of the window took out,
// work older than the window or only on a local branch. A commit withheld
// from the target is not on it. A held parent is judged by this again
// before any move.
func (s *sweepRun) parentStays(ctx context.Context, parent jira.Issue) string {
	key, status := parent.Key, parent.Status
	if item, dormant := s.r.target.ApprovedRelease.DormantFor(key, ""); dormant {
		return dormantLeftMessage(item, parent)
	}
	s.read(ctx, parent.Subtasks)
	if item, dormant := s.r.target.ApprovedRelease.DormantSubtask(parent.Subtasks); dormant {
		return fmt.Sprintf("%s stays %s: its subtask %s ships dormant", key, status, item.Key)
	}
	if slices.Contains(s.notes.SkipStatuses, status) {
		return fmt.Sprintf("%s stays %s: status is configured to be skipped", key, status)
	}
	reverted := func(k string) string {
		return fmt.Sprintf("%s stays %s: a revert of the last %d days took work of %s out, so what of it %s runs is unknown", key, status, s.conf.Days, k, s.r.name)
	}
	if sha, off := s.offTarget(key); off {
		return fmt.Sprintf("%s stays %s: its own work is not on %s: %s", key, status, s.r.name, sha)
	}
	if s.win != nil && s.win.reverted[key] {
		return reverted(key)
	}
	for _, st := range parent.Subtasks {
		issue, ok := s.issues[st]
		switch {
		case !ok:
			return fmt.Sprintf("%s stays %s: its subtask %s status is unknown", key, status, st)
		case !issue.Done:
			return fmt.Sprintf("%s stays %s: its subtask %s is %s", key, status, st, issue.Status)
		case s.win != nil && s.win.reverted[st]:
			return reverted(st)
		case len(s.items[st]) == 0:
			return fmt.Sprintf("%s stays %s: its subtask %s has no commit of the last %d days that %s or a remote branch has, so whether its work is on %[5]s is unknown", key, status, st, s.conf.Days, s.r.name)
		}
		if sha, off := s.offTarget(st); off {
			return fmt.Sprintf("%s stays %s: its subtask %s has work not on %s: %s", key, status, st, s.r.name, sha)
		}
	}
	family := uniqueSorted(append([]string{key}, parent.Subtasks...))
	if err := s.readHistory(); err != nil {
		return fmt.Sprintf("%s stays %s: its work outside the last %d days could not be read: %v", key, status, s.conf.Days, err)
	}
	if older := slices.DeleteFunc(slices.Clone(family), func(k string) bool { return !s.win.older[k] }); len(older) > 0 {
		return fmt.Sprintf("%s stays %s: %s has work older than the last %d days, so whether %s runs it is unknown", key, status, strings.Join(older, ", "), s.conf.Days, s.r.name)
	}
	if local := slices.DeleteFunc(slices.Clone(family), func(k string) bool { return !s.win.local[k] }); len(local) > 0 {
		return fmt.Sprintf("%s stays %s: %s has work only on a local branch, so whether %s runs it is unknown", key, status, strings.Join(local, ", "), s.r.name)
	}
	return ""
}

// offTarget names the first commit of the item the target does not run.
func (s *sweepRun) offTarget(key string) (string, bool) {
	i := slices.IndexFunc(s.items[key], func(c sweptCommit) bool { return !c.onTarget })
	if i < 0 {
		return "", false
	}
	return gitx.Short(s.items[key][i].sha), true
}

// readHistory reads, once a run, the work the window does not show: the
// items with a commit older than it on a remote branch or the target, and
// those with one only on a local branch, which neither has.
func (s *sweepRun) readHistory() error {
	if s.win == nil {
		s.win = &sweepWindow{since: s.since}
	}
	w := s.win
	if w.read {
		return nil
	}
	keyRe := projectKeyRe(s.r.cfg)
	keys := func(revs ...string) (map[string]bool, error) {
		commits, err := sweepLog(s.r, time.Time{}, revs...)
		if err != nil {
			return nil, err
		}
		found := map[string]bool{}
		for _, c := range commits {
			for _, k := range workKeys(keyRe, s.r.target.ApprovedRelease, c.deliveredCommit) {
				found[k] = true
			}
		}
		return found, nil
	}
	older, err := keys("--until="+w.since.Format(time.RFC3339), "--remotes", s.head)
	if err != nil {
		return fmt.Errorf("reading the history before the window: %w", err)
	}
	local, err := keys("--branches", "--not", "--remotes", s.head)
	if err != nil {
		return fmt.Errorf("reading the local branches: %w", err)
	}
	w.older, w.local, w.read = older, local, true
	return nil
}

// parentRoute is the transition a parent whose work is all on the target
// goes on by: the target's own from its status, as a deploy moves an item,
// or else the one to the done status. It is nil where the parent is to go
// nowhere: at the status that one leads to already, or where Jira offers it
// outside the done category, a done status of a target before the last that
// leads back from where the parent is. Where Jira does not say the category,
// unknown says so and the parent stays.
func (s *sweepRun) parentRoute(parent jira.Issue) (t *config.NoteTransition, unknown string) {
	notes := s.r.target.ReleaseNotes
	if moving := notes.Moving(parent.Status); moving != nil {
		return moving, ""
	}
	t = transitionBetween(notes, parent.Status, s.conf.Done)
	if t == nil || t.To == parent.Status {
		return nil, ""
	}
	if i := slices.IndexFunc(parent.Transitions, func(o jira.Transition) bool { return o.To == t.To }); i >= 0 {
		switch parent.Transitions[i].Category {
		case "done":
		case "":
			return nil, fmt.Sprintf("Jira does not say whether its move to %s is of the done category", t.To)
		default:
			return nil, ""
		}
	}
	return t, ""
}

// finishing tells whether the target's transition to the done status moves
// an item in status.
func (s *sweepRun) finishing(status string) bool {
	t := s.r.target.ReleaseNotes.Moving(status)
	return t != nil && t.To == s.conf.Done
}

// inFlow tells whether some target's transitions or approved_release name
// status; an item elsewhere, Paused or Blocked, say, stays there by hand.
func (s *sweepRun) inFlow(issue jira.Issue) bool {
	status := issue.Status
	for _, t := range s.r.cfg.Deploy.Targets {
		if n := t.ReleaseNotes; n != nil {
			for i := range n.Transitions {
				tr := &n.Transitions[i]
				if tr.AllowsType(issue.Type) && (slices.Contains(tr.From, status) || tr.To == status) {
					return true
				}
			}
		}
		if ar := t.ApprovedRelease; ar != nil && slices.Contains(ar.StatusesFor(issue.Labels), status) {
			return true
		}
	}
	return false
}

// move takes an item to the done status when it is the token owner's and,
// with approved_release, passes it: by statuses and version for an item
// itself (checked), by version alone for a parent the sweep finishes.
func (s *sweepRun) move(ctx context.Context, issue jira.Issue, why string, checked, ownershipAlready bool) {
	s.take(ctx, issue, why, checked, ownershipAlready, nil)
}

// take moves the item as move does, by t, the transition a parent goes on
// by, or, nil, on the target's way to the done status. A move by one
// transition goes through the mover of the release notes, which hands the
// item over or holds it for its comment, as a deploy would.
func (s *sweepRun) take(ctx context.Context, issue jira.Issue, why string, checked, ownershipAlready bool, t *config.NoteTransition) {
	key := issue.Key
	if item, dormant := s.r.target.ApprovedRelease.DormantFor(issue.Key, issue.Parent); dormant {
		s.judged[key] = true
		s.lines = append(s.lines, dormantLeftMessage(item, issue))
		return
	}
	if slices.Contains(s.notes.SkipStatuses, issue.Status) {
		return
	}
	stop := ""
	if t == nil {
		stop = s.typeStop(issue)
	} else if !t.AllowsType(issue.Type) {
		stop = transitionTypeStayMessage(key, issue.Status, "", t)
	}
	if stop != "" {
		s.lines = append(s.lines, stop)
		return
	}
	if !ownershipAlready && !s.owned(ctx, issue) {
		return
	}
	if s.gate != nil {
		pass, err := s.gatePass(ctx, issue, checked)
		if err != nil {
			s.app.warn("sweep %s: %s not moved: %v", s.r.name, key, err)
			if !checked && len(issue.Subtasks) > 0 {
				s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: approval could not be read: %v", key, issue.Status, why, err))
			}
			return
		}
		if !pass.ok {
			s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: %s", key, issue.Status, why, pass.why))
			return
		}
	}
	if held := s.glint.held(ctx, s.client); held != "" {
		s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: %s", key, issue.Status, why, held))
		return
	}
	to := s.conf.Done
	if t != nil {
		to = t.To
	}
	s.judged[key] = true
	if !s.apply {
		s.app.printf("sweep %s: %s would move to %s: %s", s.r.name, key, to, why)
		s.reached(issue, to)
		return
	}
	if err := s.transition(ctx, issue, t); err != nil {
		s.notMoved(issue, why, to, err)
		return
	}
	s.app.printf("sweep %s: %s moved to %s: %s%s", s.r.name, key, to, why, s.glint.takenBy())
	s.reached(issue, to)
}

// notMoved lists or warns of an item the move to to left where it was.
func (s *sweepRun) notMoved(issue jira.Issue, why, to string, err error) {
	var stay *typeStayError
	switch {
	case errors.As(err, &stay):
		s.lines = append(s.lines, stay.line)
	case errors.Is(err, errHeld):
		s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: its move to %s is held (see above)", issue.Key, issue.Status, why, to))
	default:
		s.app.warn("sweep %s: %s not moved to %s: %v", s.r.name, issue.Key, to, err)
		if len(issue.Subtasks) > 0 {
			s.lines = append(s.lines, fmt.Sprintf("%s stays %s though %s: transition to %s failed: %v", issue.Key, issue.Status, why, to, err))
		}
	}
}

// transition moves the item by t or, nil, by the way to the done status: a
// hop of a subtask with the reviewer goes as transitionDone takes it, any
// other move through the mover of the release notes.
func (s *sweepRun) transition(ctx context.Context, issue jira.Issue, t *config.NoteTransition) error {
	if t == nil {
		done, via, _ := s.doneRoute(issue)
		if via != "" || done == nil {
			return s.transitionDone(ctx, issue)
		}
		t = done
	}
	m := s.noteMover()
	if s.tried == nil {
		s.tried = map[string]bool{}
	}
	s.tried[issue.Key] = true
	return m.move(ctx, issue, t, "")
}

// reached lets the rest of the pass see the item where it went: a subtask
// done lets its parent close. The item is done when Jira offers the move
// into the done category, or, on a hop, when it reached the done status.
func (s *sweepRun) reached(issue jira.Issue, to string) {
	if i := slices.IndexFunc(issue.Transitions, func(t jira.Transition) bool { return t.To == to }); i >= 0 {
		issue.Done = issue.Transitions[i].Category == "done"
	} else {
		issue.Done = to == s.conf.Done
	}
	issue.Status = to
	s.issues[issue.Key] = issue
}

// hops tells whether transitionDone takes the item through t's first from
// before done: a subtask in a status of the reviewer's.
func (s *sweepRun) hops(issue jira.Issue, t *config.NoteTransition) bool {
	return t != nil && len(t.From) > 0 && !slices.Contains(t.From, issue.Status) && issue.Parent != "" && slices.Contains(s.review, issue.Status)
}

// doneRoute is the way transitionDone takes the item to done, with the
// transitions naming each step: a subtask with the reviewer first goes into
// via, the first from of the first transition to done, by hop, then on to
// done; any other item goes to done directly. via is "" without that step.
func (s *sweepRun) doneRoute(issue jira.Issue) (done *config.NoteTransition, via string, hop *config.NoteTransition) {
	notes := s.r.target.ReleaseNotes
	if lead := notes.Leading(s.conf.Done); s.hops(issue, lead) {
		via = lead.From[0]
		return lead, via, transitionBetween(notes, issue.Status, via)
	}
	return transitionBetween(notes, issue.Status, s.conf.Done), "", nil
}

// typeStop says why transitionDone may not move the item, "" when it may:
// a transition of its route does not move the item's type.
func (s *sweepRun) typeStop(issue jira.Issue) string {
	done, _, hop := s.doneRoute(issue)
	for _, t := range []*config.NoteTransition{hop, done} {
		if t != nil && !t.AllowsType(issue.Type) {
			return transitionTypeStayMessage(issue.Key, issue.Status, "", t)
		}
	}
	return ""
}

// transitionBetween is the transition from status to to, or failing that the
// first one leading to to: several may lead to one status from different ones.
func transitionBetween(notes *config.TargetNotes, status, to string) *config.NoteTransition {
	for i := range notes.Transitions {
		if t := &notes.Transitions[i]; t.To == to && slices.Contains(t.From, status) {
			return t
		}
	}
	return notes.Leading(to)
}

// transitionDone takes the item to the done status; a subtask in a status of
// the reviewer's goes through the from of the transition there, the way a
// reviewer would take it. Stopped between the two, it stays in that from: a
// pass finishes it by its commits, one without any is listed by nobody.
func (s *sweepRun) transitionDone(ctx context.Context, issue jira.Issue) error {
	if s.typeStop(issue) != "" {
		return errTypeNotAllowed
	}
	if _, via, _ := s.doneRoute(issue); via != "" {
		if err := s.client.Transition(ctx, issue, via); err != nil {
			return err
		}
		fresh, err := s.client.Issue(ctx, issue.Key)
		if err != nil {
			return err
		}
		issue = fresh
		// Jira may have put it elsewhere than via: the move from where it is
		// must take its type too.
		if t := transitionBetween(s.r.target.ReleaseNotes, issue.Status, s.conf.Done); t != nil && !t.AllowsType(issue.Type) {
			return &typeStayError{line: transitionTypeStayMessage(issue.Key, issue.Status, "", t)}
		}
	}
	return s.client.Transition(ctx, issue, s.conf.Done)
}

// typeStayError is a type refusal found only after the first step, where Jira
// left the item: line says where it stays, for the report.
type typeStayError struct{ line string }

func (e *typeStayError) Error() string { return errTypeNotAllowed.Error() }
func (e *typeStayError) Unwrap() error { return errTypeNotAllowed }

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
	owned, err := s.assignment(ctx, issue)
	if err != nil {
		s.app.warn("sweep %s: whose %s is is unknown: %v", s.r.name, issue.Key, err)
	}
	return owned
}

func (s *sweepRun) assignment(ctx context.Context, issue jira.Issue) (bool, error) {
	if issue.AssigneeID == s.me {
		return true, nil
	}
	return s.client.WasAssignedTo(ctx, issue.Key, s.me)
}

// heldParents judges a parent the sweep held for its handover again, as the
// sweep judges it, before the handover moves it: the transition it goes on
// by, nil for none, or the line why it stays. It reads the window of the
// commit target names once, on the first such parent; it is nil without
// release_notes.sweep.
func (a *App) heldParents(r *deployRun, target func(context.Context) (string, []string, error)) func(context.Context, jira.Issue) (*config.NoteTransition, string, error) {
	if r.target.ReleaseNotes == nil || r.target.ReleaseNotes.Sweep == nil {
		return nil
	}
	var s *sweepRun
	return func(ctx context.Context, parent jira.Issue) (*config.NoteTransition, string, error) {
		if s == nil {
			head, withheld, err := target(ctx)
			if err != nil {
				return nil, "", err
			}
			pass, err := a.sweepPass(r, head, withheld, true)
			if err != nil {
				return nil, "", err
			}
			if err := pass.connect(ctx); err != nil {
				return nil, "", err
			}
			s = pass
		}
		s.issues[parent.Key] = parent
		if line := s.parentStays(ctx, parent); line != "" {
			return nil, line, nil
		}
		t, unknown := s.parentRoute(parent)
		return t, unknown, nil
	}
}
