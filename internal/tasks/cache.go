package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aiseeq/graft/v2/internal/config"
	"github.com/aiseeq/graft/v2/internal/dotenv"
	"github.com/aiseeq/graft/v2/internal/proc"
	"github.com/aiseeq/graft/v2/internal/stats"
	"github.com/aiseeq/graft/v2/internal/taskcache"
)

// NoCacheEnv set to 1 runs every task as if it had no cache, in graft
// processes started by the run too.
const NoCacheEnv = "GRAFT_NO_CACHE"

// cached looks the task up in the cache: a hit is reported and recorded.
// The key is returned to keep the pass of a run; an error means the task
// runs without the cache.
func (r *Runner) cached(ctx context.Context, name string, args []string) (key string, hit bool, err error) {
	if key, err = r.cacheKey(ctx, name, args); err != nil {
		return "", false, err
	}
	dir, err := taskcache.Dir()
	if err != nil {
		return "", false, err
	}
	e, ok, err := taskcache.Lookup(dir, key)
	if err != nil || !ok {
		return key, false, err
	}
	fmt.Fprintf(r.Stderr, "graft: %s: passed on this tree %s, skipped (cache; --no-cache runs it)\n", name, e.Time.Local().Format("2006-01-02 15:04"))
	outermost := name
	if len(r.frames) > 0 {
		outermost = r.frames[0].task
	}
	use := stats.Usage{Wall: seconds(e.WallS), User: seconds(e.UserS), Sys: seconds(e.SysS)}
	r.write(stats.Record{Kind: stats.KindCacheHit, Task: name}, use, outermost)
	return key, true, nil
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// keep stores the pass of a task run with its costs, unless what the key
// covers changed while it ran: the run then passed on something else.
func (r *Runner) keep(ctx context.Context, key, name string, args []string, use stats.Usage) {
	after, err := r.cacheKey(ctx, name, args)
	if err == nil && after != key {
		fmt.Fprintf(r.Stderr, "graft: %s: passed, but the tree or its inputs changed while it ran: not kept in the cache\n", name)
		return
	}
	var dir string
	if err == nil {
		dir, err = taskcache.Dir()
	}
	if err == nil {
		err = taskcache.Store(dir, key, taskcache.Entry{Project: r.Root, Task: name, Time: time.Now(),
			WallS: use.Wall.Seconds(), UserS: use.User.Seconds(), SysS: use.Sys.Seconds()})
	}
	if err != nil {
		fmt.Fprintf(r.Stderr, "graft: %s: passed, but the pass is not kept in the cache: %v\n", name, err)
	}
}

// cacheKey is the hash of everything the result of the task depends on: the
// work tree without cache_exclude and the version files, the definition of
// the task with its deps and inner tasks, the values of their env and .env
// keys, the commit message in the gate when they name its file, the
// arguments, the cache inputs, and this graft. GRAFT_VERSION is left out: it changes with
// every commit.
func (r *Runner) cacheKey(ctx context.Context, name string, args []string) (string, error) {
	h := sha256.New()
	add := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s\n", len(p), p)
		}
	}
	add("graft task cache 1", r.GraftVersion, runtime.GOOS, runtime.GOARCH, name, fmt.Sprint(len(args)))
	add(args...)
	repo, err := r.repo()
	if err != nil {
		return "", err
	}
	paths := r.Config.CachePaths(name)
	tree, err := repo.ContentID(func(path string) bool {
		if r.Config.CacheExcluded(path) {
			return true
		}
		if len(paths) == 0 {
			return false
		}
		for _, re := range paths {
			if re.MatchString(path) {
				return false
			}
		}
		return true
	})
	if err != nil {
		return "", fmt.Errorf("reading the work tree: %w", err)
	}
	add(tree)
	readsMessage := false
	if err := r.keyDefinitions(add, name, map[string]bool{}, &readsMessage); err != nil {
		return "", err
	}
	if path, ok := r.Extra[MessageFileEnv]; ok && readsMessage {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("the commit message for the cache key: %w", err)
		}
		add("message", string(data))
	}
	if err := r.keyInputs(ctx, add, r.Config.Tasks[name].Cache); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// keyDefinitions adds the task, its deps and inner tasks: each definition as
// configured and the values its env and .env keys take now. readsMessage is
// set when one of them names the commit message file: only then does the
// message count, or no gate of a new commit would ever skip.
func (r *Runner) keyDefinitions(add func(...string), name string, seen map[string]bool, readsMessage *bool) error {
	if seen[name] {
		return nil
	}
	seen[name] = true
	t := r.Config.Tasks[name]
	def, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("task %s for the cache key: %w", name, err)
	}
	add("task", name, string(def))
	if namesMessage(def) {
		*readsMessage = true
	}
	for _, k := range sortedKeys(t.Env) {
		v, err := dotenv.Expand(t.Env[k], r.lookup)
		if err != nil {
			return fmt.Errorf("task %s: env %s: %w", name, k, err)
		}
		add(k, v)
	}
	values, err := KeyValues(r.lookup, t.Keys)
	if err != nil {
		return fmt.Errorf("task %s: %w", name, err)
	}
	add(values...)
	if t.TestDB {
		db, err := json.Marshal(r.Config.TestDB)
		if err != nil {
			return fmt.Errorf("test_db for the cache key: %w", err)
		}
		add("test_db", string(db))
	}
	for _, d := range t.Deps {
		if err := r.keyDefinitions(add, d, seen, readsMessage); err != nil {
			return err
		}
	}
	for _, s := range t.Run {
		if s.Task != "" {
			if err := r.keyDefinitions(add, s.Task, seen, readsMessage); err != nil {
				return err
			}
		}
	}
	return nil
}

// keyInputs adds the inputs from outside the tree the cache declares.
func (r *Runner) keyInputs(ctx context.Context, add func(...string), c *config.TaskCache) error {
	for _, k := range c.Env {
		v, ok := os.LookupEnv(k)
		add("env", k, fmt.Sprint(ok), v)
	}
	for _, f := range c.Files {
		path, err := r.inputPath(f)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			add("file", f, "absent")
		case err != nil:
			return fmt.Errorf("cache.files %s: %w", f, err)
		default:
			add("file", f, string(data))
		}
	}
	for _, c := range c.Commands {
		// A command that may not start here leaves the task uncached:
		// starting it would only raise an SELinux alert.
		if err := proc.RefusedCommand(r.Root, c.Argv[0]); err != nil {
			return fmt.Errorf("cache.commands %s: %w", c.Source, err)
		}
		cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
		cmd.Dir = r.Root
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("cache.commands %s: %w: %s", c.Source, err, strings.TrimSpace(stderr.String()))
		}
		add("command", c.Source, string(out))
	}
	return nil
}

// inputPath is a cache.files entry made absolute: ~/ is the home directory,
// a relative path is under the work tree.
func (r *Runner) inputPath(f string) (string, error) {
	if rest, ok := strings.CutPrefix(f, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("input %s: %w", f, err)
		}
		return filepath.Join(home, rest), nil
	}
	if filepath.IsAbs(f) {
		return f, nil
	}
	return filepath.Join(r.Root, filepath.FromSlash(f)), nil
}

// namesMessage says whether a task's JSON definition names the commit
// message file: a step may read it.
func namesMessage(def []byte) bool {
	return strings.Contains(string(def), MessageFileEnv)
}
