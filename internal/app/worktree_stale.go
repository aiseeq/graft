package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/gitx"
)

// WorktreeStaleOptions are the thresholds and extra catalogs for worktree stale.
type WorktreeStaleOptions struct {
	Uncommitted, Unmerged       time.Duration
	UncommittedSet, UnmergedSet bool
	Here                        bool
	Trunk                       string
	In                          []string
}

type staleWorktree struct {
	path         string
	branch, head string
	prunable     bool
	repo         *gitx.Repo
}

type staleTrunk struct {
	ref     string
	why     string
	exclude []string
}

type staleCatalogRoot struct {
	path     string
	required bool
}

// WorktreeStale reports old changes in the project, its registered worktrees,
// and graft's worktree cache. It does not fetch or update repository refs.
func (a *App) WorktreeStale(ctx context.Context, o WorktreeStaleOptions) (retErr error) {
	if err := unbindGitFromCaller(); err != nil {
		return err
	}
	previousLazyFetch, hadLazyFetch := os.LookupEnv("GIT_NO_LAZY_FETCH")
	if err := os.Setenv("GIT_NO_LAZY_FETCH", "1"); err != nil {
		return fmt.Errorf("disable Git lazy fetch: %w", err)
	}
	defer func() {
		var restoreErr error
		if hadLazyFetch {
			restoreErr = os.Setenv("GIT_NO_LAZY_FETCH", previousLazyFetch)
		} else {
			restoreErr = os.Unsetenv("GIT_NO_LAZY_FETCH")
		}
		if restoreErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("restore GIT_NO_LAZY_FETCH: %w", restoreErr))
		}
	}()
	var (
		trees []staleWorktree
		err   error
	)
	if o.Here {
		trees, err = discoverCurrentStaleWorktree(a.Dir)
	} else {
		trees, err = discoverStaleWorktrees(a.Dir, o.In)
	}
	if err != nil {
		return err
	}
	return a.reportStaleWorktrees(ctx, o, trees)
}

func discoverCurrentStaleWorktree(dir string) ([]staleWorktree, error) {
	repo, err := gitx.Open(dir)
	if errors.Is(err, gitx.ErrNotRepo) {
		return nil, &ExitCodeError{Code: 2, Err: fmt.Errorf("--here requires a Git worktree")}
	}
	if err != nil {
		return nil, err
	}
	current, err := canonicalWorktreePath(repo.Root)
	if err != nil {
		return nil, err
	}
	listed, err := listWorktrees(repo)
	if err != nil {
		return nil, fmt.Errorf("list worktrees in %s: %w", repo.Root, err)
	}
	for _, w := range listed {
		path, err := canonicalWorktreePath(w.path)
		if err != nil {
			return nil, err
		}
		if path == current {
			return []staleWorktree{{path: path, branch: w.branch, head: w.head, prunable: w.prunable, repo: repo}}, nil
		}
	}
	return nil, fmt.Errorf("current Git worktree %s is not registered", current)
}

func discoverStaleWorktrees(dir string, extraCatalogs []string) ([]staleWorktree, error) {
	rootRepo, err := gitx.Open(dir)
	if err != nil {
		return nil, err
	}
	trees := map[string]staleWorktree{}
	if err := addRepoWorktrees(trees, rootRepo); err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("locating the cache: %w", err)
	}
	catalogs := []staleCatalogRoot{{path: filepath.Join(cache, "worktrees")}}
	for _, path := range extraCatalogs {
		catalogs = append(catalogs, staleCatalogRoot{path: path, required: true})
	}
	seenCatalogs := map[string]bool{}
	for _, item := range catalogs {
		catalog, err := filepath.Abs(item.path)
		if err != nil {
			return nil, fmt.Errorf("resolving catalog %s: %w", item.path, err)
		}
		if seenCatalogs[filepath.Clean(catalog)] {
			continue
		}
		seenCatalogs[filepath.Clean(catalog)] = true
		roots, err := worktreeCatalogRoots(catalog, item.required)
		if err != nil {
			return nil, err
		}
		for _, path := range roots {
			repo, err := gitx.Open(path)
			if err != nil {
				return nil, fmt.Errorf("open worktree in %s: %w", path, err)
			}
			if err := addRepoWorktrees(trees, repo); err != nil {
				return nil, err
			}
		}
	}

	ordered := make([]staleWorktree, 0, len(trees))
	for _, tree := range trees {
		ordered = append(ordered, tree)
	}
	slices.SortFunc(ordered, func(a, b staleWorktree) int { return strings.Compare(a.path, b.path) })
	return ordered, nil
}

func (a *App) reportStaleWorktrees(ctx context.Context, o WorktreeStaleOptions, ordered []staleWorktree) error {
	trunks := map[string]staleTrunk{}
	found := 0
	verificationIssue := false
	for _, tree := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		details, notes, err := staleWorktreeDetails(ctx, tree, o, trunks)
		if err != nil {
			return err
		}
		if len(details) == 0 && len(notes) == 0 {
			continue
		}
		verificationIssue = verificationIssue || len(notes) != 0
		label := staleTreeLabel(tree)
		if len(details) != 0 {
			found++
			a.printf("stale     %s: %s", escapeUnprintable(label), strings.Join(append(details, notes...), "; "))
		} else {
			a.printf("checked   %s: %s", escapeUnprintable(label), strings.Join(notes, "; "))
		}
	}
	if verificationIssue {
		return &ExitCodeError{Code: 2, Err: fmt.Errorf("unmerged check incomplete; resolve the trunk or specify --trunk <ref>")}
	}
	if found == 0 {
		a.printf("nothing stale")
		return nil
	}
	return &ExitCodeError{Code: 1, Err: fmt.Errorf("%d stale worktrees", found)}
}

func staleWorktreeDetails(ctx context.Context, tree staleWorktree, o WorktreeStaleOptions, trunks map[string]staleTrunk) ([]string, []string, error) {
	var details, notes []string
	if o.UncommittedSet && !tree.prunable {
		count, oldest, err := staleFileCount(tree.path, o.Uncommitted)
		if err != nil {
			return nil, nil, err
		}
		if count > 0 {
			details = append(details, fmt.Sprintf("%d uncommitted files, oldest %s; commit or discard the changes", count, oldest.Round(time.Second)))
		}
	}
	if o.UnmergedSet && (tree.head != "" || o.Trunk != "") {
		unmerged, note, err := staleTreeUnmerged(ctx, tree, o.Unmerged, o.Trunk, trunks)
		if err != nil {
			return nil, nil, err
		}
		details = append(details, unmerged...)
		if note != "" {
			notes = append(notes, note)
		}
	}
	return details, notes, nil
}

func staleTreeUnmerged(ctx context.Context, tree staleWorktree, threshold time.Duration, trunkFlag string, trunks map[string]staleTrunk) ([]string, string, error) {
	common := filepath.Clean(tree.repo.CommonDir)
	trunk, ok := trunks[common]
	if !ok {
		trunk = findLocalStaleTrunk(ctx, tree.repo, trunkFlag)
		trunks[common] = trunk
	}
	if trunk.why != "" {
		return nil, "unmerged check skipped: " + trunk.why + "; specify --trunk <ref>", nil
	}
	if tree.head == "" {
		return nil, "", nil
	}
	merged, count, age, err := staleMergeStatus(ctx, tree.repo, trunk, tree.head, time.Now())
	if err != nil {
		return nil, "", fmt.Errorf("check %s against %s: %w", tree.path, trunk.ref, err)
	}
	if merged || age < threshold {
		return nil, "", nil
	}
	return []string{fmt.Sprintf("%d unmerged commits, last commit %s old; %s", count, age.Round(time.Second), staleBranchHint(tree.prunable))}, "", nil
}

func staleTreeLabel(tree staleWorktree) string {
	if !tree.prunable {
		return tree.path
	}
	return "branch " + tree.branch + " (prunable worktree " + tree.path + ")"
}

func staleBranchHint(prunable bool) string {
	if prunable {
		return "review or remove the branch"
	}
	return "merge or remove the branch"
}

func addRepoWorktrees(trees map[string]staleWorktree, repo *gitx.Repo) error {
	listed, err := listWorktrees(repo)
	if err != nil {
		return fmt.Errorf("list worktrees in %s: %w", repo.Root, err)
	}
	for _, w := range listed {
		path, err := canonicalWorktreePath(w.path)
		if err != nil {
			return err
		}
		if _, exists := trees[path]; exists {
			continue
		}
		trees[path] = staleWorktree{path: path, branch: w.branch, head: w.head, prunable: w.prunable, repo: repo}
	}
	return nil
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving worktree %s: %w", path, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolving worktree %s: %w", path, err)
	}
	return filepath.Clean(abs), nil
}

func worktreeCatalogRoots(catalog string, required bool) ([]string, error) {
	if _, err := os.Stat(catalog); errors.Is(err, os.ErrNotExist) && !required {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("worktree catalog %s: %w", catalog, err)
	}
	var roots []string
	err := filepath.WalkDir(catalog, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		marker := filepath.Join(path, ".git")
		if _, err := os.Lstat(marker); err == nil {
			repo, openErr := gitx.Open(path)
			if openErr != nil {
				if isDanglingWorktreeGitfile(path) {
					return filepath.SkipDir
				}
				return fmt.Errorf("open repository marker %s: %w", marker, openErr)
			}
			if filepath.Clean(repo.Root) == filepath.Clean(path) {
				roots = append(roots, path)
			}
			return filepath.SkipDir
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan worktree catalog %s: %w", catalog, err)
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

func isDanglingWorktreeGitfile(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	gitDir, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return false
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	_, err = os.Stat(gitDir)
	return errors.Is(err, os.ErrNotExist)
}

func staleFileCount(root string, threshold time.Duration) (int, time.Duration, error) {
	repo, err := gitx.Open(root)
	if err != nil {
		return 0, 0, fmt.Errorf("open worktree %s: %w", root, err)
	}
	files, err := staleChangedFiles(repo, map[string]bool{})
	if err != nil {
		return 0, 0, err
	}
	now := time.Now()
	count := 0
	var oldest time.Duration
	for _, file := range files {
		age := now.Sub(file.mtime)
		if age >= threshold {
			count++
			oldest = max(oldest, age)
		}
	}
	return count, oldest, nil
}

type staleFile struct {
	path  string
	mtime time.Time
}

func staleChangedFiles(repo *gitx.Repo, visited map[string]bool) ([]staleFile, error) {
	common := filepath.Clean(repo.CommonDir)
	if visited[common] {
		return nil, nil
	}
	visited[common] = true
	paths, err := repo.ChangedFilesReadOnly()
	if err != nil {
		return nil, fmt.Errorf("read changes in %s: %w", repo.Root, err)
	}
	var files []staleFile
	for _, name := range paths {
		changed, err := staleChangedPath(repo, name, common, visited)
		if err != nil {
			return nil, err
		}
		files = append(files, changed...)
	}
	return files, nil
}

func staleChangedPath(repo *gitx.Repo, name, common string, visited map[string]bool) ([]staleFile, error) {
	path := filepath.Join(repo.Root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", displayName(path), err)
	}
	if !info.IsDir() {
		return []staleFile{{path: path, mtime: info.ModTime()}}, nil
	}
	marker := filepath.Join(path, ".git")
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		return []staleFile{{path: path, mtime: info.ModTime()}}, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat %s: %w", marker, err)
	}
	sub, err := gitx.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open submodule %s: %w", path, err)
	}
	if filepath.Clean(sub.Root) != filepath.Clean(path) || filepath.Clean(sub.CommonDir) == common {
		return []staleFile{{path: path, mtime: info.ModTime()}}, nil
	}
	return staleChangedFiles(sub, visited)
}

func findLocalStaleTrunk(ctx context.Context, repo *gitx.Repo, trunkFlag string) staleTrunk {
	cfg := &config.Config{}
	if trunkFlag == "" {
		mainCfg, err := mainWorktreeConfig(repo)
		if err != nil {
			return staleTrunk{why: "cannot read main worktree config: " + err.Error()}
		}
		cfg = mainCfg
	}
	remotes, err := repo.Remotes()
	if err != nil {
		return staleTrunk{why: "cannot list remotes: " + err.Error()}
	}
	ref, err := trunkRef(repo, cfg, trunkFlag, remotes)
	if err != nil {
		return staleTrunk{why: "local trunk is unavailable: " + err.Error()}
	}
	ok, err := repo.RevExists(ref)
	if err != nil {
		return staleTrunk{why: "cannot check local trunk " + ref + ": " + err.Error()}
	}
	if !ok {
		return staleTrunk{why: "local trunk " + ref + " is unavailable"}
	}
	c := &cleaner{ctx: ctx, app: &App{}, repo: repo, cfg: cfg, trunk: ref, counts: map[string]int{}}
	trunkCfg, err := c.readTrunkConfig()
	if err != nil {
		return staleTrunk{why: "cannot read trunk rules: " + err.Error()}
	}
	for _, file := range trunkCfg.VersionFiles() {
		c.exclude = append(c.exclude, ":(exclude)"+file)
	}
	return staleTrunk{ref: ref, exclude: c.exclude}
}

func staleMergeStatus(ctx context.Context, repo *gitx.Repo, trunk staleTrunk, rev string, now time.Time) (bool, int, time.Duration, error) {
	countOut, err := repo.Git("rev-list", "--count", trunk.ref+".."+rev)
	if err != nil {
		return false, 0, 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(countOut))
	if err != nil {
		return false, 0, 0, fmt.Errorf("count commits %s..%s: %q: %w", trunk.ref, rev, countOut, err)
	}
	if count == 0 {
		return true, 0, 0, nil
	}
	dateOut, err := repo.Git("log", "-1", "--format=%ct", trunk.ref+".."+rev)
	if err != nil {
		return false, 0, 0, err
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(dateOut), 10, 64)
	if err != nil {
		return false, 0, 0, fmt.Errorf("last commit date %q: %w", dateOut, err)
	}
	age := now.Sub(time.Unix(sec, 0))
	c := &cleaner{ctx: ctx, app: &App{}, repo: repo, cfg: &config.Config{}, trunk: trunk.ref, exclude: trunk.exclude, counts: map[string]int{}}
	if err := c.readSubjects(); err != nil {
		return false, count, age, err
	}
	result, err := c.verdict(rev)
	return result.merged, count, age, err
}
