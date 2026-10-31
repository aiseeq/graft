# graft

One command line for the development workflow of every repository you work
in, in place of a Makefile and shell glue. `graft commit` runs the
project's gate, bumps the version, stages everything, scans the staged changes
for secrets and oversized files, commits with the work item key taken from the
branch name, and pushes to every remote. Hooks installed by `graft init` keep
plain `git commit` out while letting git's own merges, rebases and
cherry-picks through the same gate. Around the commit: project tasks with
dependencies and locks, background services, a disposable test database,
pinned tools, the event journal (flags) and a deploy wrapper.

What differs between projects lives in `.graft.yaml` at the repository root.
graft is a single Go binary with no shell dependency in its core, so it works the
same on Linux, macOS and Windows (Git for Windows).

## Install

```sh
git clone https://github.com/aiseeq/graft && cd graft
echo GRAFT_PILOT=$HOME/src/some-project > .env   # a project of yours that uses graft
go run . install    # builds, checks, runs the pilot's gate, atomically replaces ~/bin/graft
```

and later `git pull && go run . install` in that checkout. Every project's
hooks run `~/bin/graft`, so before replacing it `install` runs the gate of
one project, the pilot (`GRAFT_PILOT` in `.env`, which git ignores), with
the new binary first on `PATH`; a failing gate leaves `~/bin/graft` as it
was. Without a pilot it refuses; `go run . install -- --no-pilot` skips it,
for a first install or a machine with no project yet. Put `~/bin` on
`PATH`. `go install github.com/aiseeq/graft/v2@latest` works too, but puts a
second graft in GOBIN (`~/go/bin`): the hooks run whichever comes first on
`PATH`, so keep one. Requires git 2.31 or newer. A project can require a graft version with
`graft: ">=X.Y.Z"` in `.graft.yaml`: an older graft refuses to work on it.
The install hints install the latest: graft versions are not pinned.

Settings that belong to the person rather than to a project live in
`<user config dir>/graft/config.yaml` (`~/.config/graft/config.yaml` on
Linux):

```yaml
jira:
  env_file: ~/secrets/jira.env   # deploy release notes: JIRA_BASE_URL, JIRA_EMAIL, JIRA_API_TOKEN
tools:
  bin_dir: ~/bin                 # where graft tools install puts binaries; default: go install's GOBIN
leaks:                           # keep private names out of public repositories (see Checks)
  public_remotes: [github.com/someone/]       # a repository with a remote under one of these is public
  private_remotes: [github.com/someone/secret-tool]  # private even under a public prefix: the narrower prefix wins
  private_sources: [~/work/projectA]          # private projects: their Go declarations are private names
  terms_file: ~/.config/graft/private-terms   # "kind: regexp" per line: keys, domains, addresses, names
  min_name_length: 10                         # default 10
  allow:
    - {term: SomeName, reason: why this name is fine everywhere}
load:                            # heavy tasks on a shared machine (heavy: true in a task)
  wait_above: 22                 # default: the machine's threads; heavy tasks start while fewer tasks are ready to run
  max_io_full: 9.6               # default 9.6: % of the last 5s all processes stalled on the disk; 100 leaves the disk out
  max_mem_some: 10               # default 10: % of the last 10s some process stalled on memory (/proc/pressure/memory)
  max_swap_kbs: 1024             # default 1024: swap traffic in and out, KB/s (pswpin + pswpout of /proc/vmstat), busy only with memory pressure
  swap_window: 1s                # default 1s: how long a starting task measures the swapping
  threads: 4                     # default 4: {threads} in steps, GRAFT_THREADS and GOMAXPROCS in heavy tasks
  settle: 10s                    # default 10s: a heavy task that starts gives its load this long to show before the next
  poll: 5s                       # default 5s; 0 anywhere here means the default
limits:                          # graft stats marks the tasks over them, by median without the waits; a run over says so when it ends
  gate: 1m                       # default 1m: the gate of a commit as a whole
  task: 8m                       # default 8m: any other task, a test run before a deploy, merge or release
commit:
  conventional: true             # default false: graft commit refuses a subject that is not Conventional Commits
  local_branches: ['nightly/*']  # a plain git commit on these passes the hook after the checks: no gate, version or push
modules:
  latest: [github.com/someone/, example.org/team/] # raise these modules before graft commit and graft gate
remote_heavy:
  host: pc
  up: [scripts/remote.sh, up]
  run: [scripts/remote.sh, run]
  threads: 16
  when: busy                    # default for user config; also: always
  broken_exit: 125              # run's transport failure status
install:
  hosts: [pc]                   # copy installed binaries to the same absolute path
```

With `commit.conventional` graft commit wants the subject as `type(scope)!:
description`, the type one of build, chore, ci, docs, feat, fix, perf,
refactor, revert, style, test; git's own `Revert "..."` passes. It is the
user's, so it holds in every project they commit to.

`commit.local_branches` are branches whose commits stay on the machine until
the project merges them, such as a batch of edits a scheduled tool leaves for
the project to look at: there a plain `git commit` passes the pre-commit hook
after the checks (secrets, large files), without the gate and the version
bump, and nothing is pushed. Merging the branch brings the gate (see Hooks).

`modules.latest` lists Go module path prefixes. Before `graft commit` runs its
gate and before a manual `graft gate`, graft raises matching requirements in
tracked `go.mod` files to the latest versions visible through the user's Go
proxy settings, then tidies the module and refreshes an existing vendor tree.
Offline or otherwise unknown latest versions produce a warning and leave that
module file alone. If a raised version makes the gate fail, the commit stops
and the raised files stay in the work tree for repair. Without this setting,
module versions are left alone.

A heavy task, and the gate as a whole, starts when the machine is not busy,
by the rule of `github.com/aiseeq/akit/pressure`: the tasks ready to run at
this moment (`procs_running` of `/proc/stat`, one reading, not an average
that lags a minute behind) below `wait_above`, no disk freeze going on and
the IO pressure of the last 5 seconds below `max_io_full`, and not the
memory pressure above `max_mem_some` while the machine swaps more than
`max_swap_kbs`: swap in use is no harm, and swapping without memory pressure
neither. The disk is read from the disk state source on
`$XDG_RUNTIME_DIR/io-state.sock`, asked for the task's kind (its `profile`
in the stats) and giving back a profile of it, which the task's stats line
keeps as `disk_profile`; without the source, or when it fails, graft says so
once per look at the machine (a task with `remote_heavy` under `when: busy`
looks twice: before it chooses the host, and when it waits here) and
measures the IO pressure by the `full` total of `/proc/pressure/io` over
`swap_window` (between readings while it waits); a system without that file
either leaves the disk unchecked, and says so. The quiet mode of `busy --quiet-until TIME`
or `busy --quiet DURATION` (akit) makes the machine busy until that time,
whatever it measures: the wait says "quiet mode until HH:MM" (the date too
when it is not today), and `busy --quiet-off` ends it sooner. A priority task
of `graft deploy` waits for it no longer than `deploy.load_wait` allows, as
for any busy machine. A measure akit reports and graft does not know yet
makes the machine busy too, by its own words. A measure the system lacks (no `/proc/stat`,
a kernel without PSI) is skipped and said. Heavy tasks of
all projects, on a calm machine too, queue under one lock in the user cache
(`graft/load.lock`) in ticket order and start one at a time: a task that
starts holds the queue for `settle`, or until it ends if sooner, so the next
one measures the machine with its load on it; tasks that measured together
would all find the same calm machine and start together. A heavy task inside
a heavy task or the gate was admitted with it, and so is a graft that the
steps of an admitted task start (they carry `GRAFT_ADMITTED`). Priority tasks
from `graft deploy` bypass the queue and may start ahead of queued tasks.
Ctrl-C stops the wait.

The steps of a heavy task and of the gate, and a step with `{threads}`, get `GOMAXPROCS` set to `threads`
unless the task's `env` or the environment sets it: one Go program does not take every thread of
the machine, the go command running a compile per package at once, each with
as many threads as `GOMAXPROCS`. `GOFLAGS` in a task's `env` comes after the
user's (the environment's, else the go env file's), which it would otherwise
replace; a later flag wins, so the task's still override them.

A heavy command that is no task of the project (a one-off `go test`, a
build, an e2e run) takes the same queue with `graft exec --heavy -- <command>
[args]`: it runs in the current directory once the machine admits it, `{threads}`
in its arguments and `GRAFT_THREADS` set to `threads`, and graft exits with
its exit status, 128 and the signal for one a signal ended. Its own failures
exit as `env`'s do, with a `graft: …` line, so a caller tells them from the
command's: 125 graft itself (a usage error, `-C`, the config, the queue,
`remote_heavy`'s `run` that does not start, an interrupt that ended the
command), 126 a command found but not started, 127 one not found, these two
with the line `graft: exec: start <command>: …`, as a command of its own
that exits 126 or 127 has none. On the
host of `remote_heavy` the status is the one its `run` gives, so a failing
`ssh` says 255. `graft stats` records it as the task `exec` of the work tree
around (`--task exec` lists the commands); without `--heavy` it runs at once
and is only recorded. `--settle <duration>` holds a separate FIFO lock for
that long after the command starts, or until it ends if sooner. Only another
`exec --settle` waits for this lock; other heavy tasks use the ordinary queue
and `load.settle`. This is for a command whose load comes later than its start,
such as an agent that takes a minute to get going.

`graft exec --heavy -- graft <task>` takes no queue slot itself: it starts the
inner graft with `GRAFT_EXEC_HEAVY=1`, by which that graft runs its top-level
task, or the command of its `graft exec`, as heavy (placed on the host or
admitted here once, as any heavy task), even when `.graft.yaml` does not mark
it heavy; the inner graft's own steps do not inherit the variable. Other
graft commands than a task and `exec` run unadmitted there; `gate` and
`commit` admit their gate themselves.

A project with another machine for heavy work names it in `.graft.yaml`, and
the steps of its heavy tasks and its `graft exec --heavy` go there first,
skipping this machine's queue. A project's `when` defaults to `always`. The
same fields may be set in the user config; a project's setting replaces the
whole user setting. In user config, `when` defaults to `busy`: graft reads
the load rule once, and whether another heavy task holds the queue (starting,
or settling after its start), and sends eligible work to the host only when
this machine's heavy queue would wait. On a calm machine it runs locally without
a host line. Set `when: always` to send eligible work every time.

```yaml
remote_heavy:
  host: pc                       # the machine, in graft's lines and in stats
  up: [scripts/remote.sh, up]          # exits 0 when the host takes commands, within 30s
  run: [scripts/remote.sh, run]        # runs the appended argv on the host from the work tree root; with {root}, from the step's directory
  threads: 16                    # {threads} and GRAFT_THREADS there; 0 leaves GRAFT_THREADS unset and refuses {threads}
  when: always                   # project default; user config defaults to busy
  broken_exit: 125               # run's transport failure status
```

The project's `run` carries the tree to the host and runs the command there;
graft only decides where a step runs and appends `env GRAFT_HEAVY=local
K=V… <argv>`: the variables it sets for the step (`env`, `GRAFT_VERSION`,
`GRAFT_THREADS`), since the host does not get this machine's environment,
and `GRAFT_HEAVY=local`, so that a graft the step starts there keeps its heavy
work. `run` starts from the work tree root; when it names `{root}`, filled
with the work tree root, it starts from the step's directory instead (a
task's `dir`, `graft exec` in a subdirectory), as a `run` that carries the
whole tree and runs the command where it was started needs. A step
interrupted here sends `run` SIGTERM, so it can take its work off the host,
and kills it 30 seconds later. `up` runs once per graft, at the first heavy task. With user-configured
`remote_heavy`, only tasks whose every step is a `go test` or `go vet` command
that writes no output file can use the host; a task with `remote: true` can
send any heavy work. Output flags such as `-o`, `-coverprofile` and `-trace`
(also as `-test.coverprofile`), `-fuzz` and `-mod=mod` keep the task here, in the step or in the arguments given for `{args}`: a
`go test -v {args} ./...` task goes with `-run TestX` and stays with
`-coverprofile=c.out`. A heavy task that would not queue here (a graft a step
of an admitted heavy task starts, a heavy task inside another) does not
overflow under `when: busy`. Project configuration keeps its existing rule: every
heavy task can go to its host except tasks that need local files or data.
The host's command gets the task's `env`, `GRAFT_HEAVY=local`, the threads
and the caller's `GOOS`, `GOARCH`, `GOFLAGS`, `CGO_ENABLED` and
`GOEXPERIMENT` (the task's own value wins), not the rest of the caller's
environment: `FLAG=1 graft exec --heavy -- go test ./...` does not see
`FLAG` there. Pass such a variable with `graft exec --heavy --env FLAG --
…` (repeatable; it sets `GRAFT_PASS_ENV`, a comma-separated list the grafts
the command starts read too), or keep the run here with `graft exec --heavy
--local` or `GRAFT_HEAVY=local`.
A gate under the user config's `when: busy`, in a project without its own
`remote_heavy`, that would wait here while the host answers and has a task
with `remote: true` (the project vouches that its tests pass on the host:
they read nothing outside the tree that only this machine has, a tool on
`PATH` or state under the home directory), runs split: `graft: gate: busy
here: go test and go vet steps of remote: true tasks on pc, the rest waits
for this machine once`. It takes no admission as a whole, nor do its
heavy tasks; every `go test` and `go vet` step that writes no file of a
`remote: true` task, at any depth, runs on the host unless the task would
stay here (test database, `.env` keys, a directory the host's `run` cannot
reach), and every other step (the gate's own go vet, the gofmt check, go
fix, installs, scripts, the go steps of the other tasks) runs here under
one admission: the first of them waits for this machine, the rest run under
its admission to the gate's end, its queue lock gone after `load.settle` as
any task's. A gate without such a task waits here whole. A host step
whose path breaks is said and rerun here under that admission, and the
gate's later go steps run here too. A gate whose host does not answer waits
here whole and says why.
Each heavy task
says where it runs, `graft: build: heavy, on pc`, or why it stays here and
queues as before, `graft: build: heavy, on this machine: <why>`:

- `GRAFT_HEAVY=local` in the environment (any other value is an error), or
  `graft exec --heavy --local`, which sets it for the command too;
- `remote: false` in the task: its steps write files needed here;
- the task uses the test database, a jest or playwright report file, `.env`
  keys (`dotenv`, `dotenv_sets`) or a `${KEY}` that is no variable of the
  task, or names `GRAFT_COMMIT_MESSAGE_FILE` in a gate: none of these is on
  the host;
- the task runs outside the work tree root (`dir`, `graft exec` in a
  subdirectory) and `run` names no `{root}`;
- the task names `cache.files`: it reads files outside the tree or ignored
  by git, and the host has none of them;
- `up` failed or timed out; its first line of output is in the reason.

For a user-configured host, an `up` failure or a `run` exit of `broken_exit`
means the transport path broke, so graft writes the reason and runs the task
here through the heavy queue. The same fallback applies to `graft exec --heavy`.
It appends one JSON line per occurrence to
`<user cache dir>/graft/remote-broken.jsonl`, beside `stats.jsonl`, with the
time, project, task, host, reason and process/session identifiers. A different
`run` exit is the task's failure and is not retried.

A heavy task inside a heavy task decides for itself, and a task that is not
heavy runs here wherever its caller runs.

`graft deploy` gives its heavy tasks, and those of every graft its script
starts, a priority (`GRAFT_LOAD_PRIORITY`, `GRAFT_LOAD_BUDGET`,
`GRAFT_LOAD_THEN`): they skip the queue and the settling, a deploy not
standing behind the test runs of every project, and wait for the machine
out of one budget for the whole deploy, `deploy.load_wait`; when it is spent
they start, or with `then: fail` fail the deploy. Only waiting spends the
budget, the waits of tasks side by side adding up: a task that finds the
machine calm late in a long deploy takes nothing from it. The budget lives in
a file in the user cache (`graft/load-budget-*`) for as long as the deploy
runs. The wait shows in `graft
stats` (`wait_s`).

## Commands

| Command | What it does |
|---|---|
| `graft commit -m "fix: ..."` | lock, stage work, gate, stage paths changed when it started, bump patch (version mode `file`), stage bump, checks, commit, push to every remote |
| `graft commit --minor` / `--major` | same with a minor / major bump (version mode `file` only) |
| `graft commit -F msg.txt` / `-F -` | message from a file / from stdin, byte for byte |
| `graft amend` | lock, stage work before the gate, gate, stage paths changed since the amended commit's parent (including the amended commit's files), `ticket.required` on the whole amended commit, checks, `git commit --amend --no-edit`, no push; restores the original index if the gate or a later step fails or the gate leaves the amended commit as its parent has it, and refuses a commit that is already on a remote unless only on branches in `push.force_with_lease` |
| `graft release` | tag the pushed HEAD with the version, push the tag to every remote |
| `graft release --minor` / `--major` / `--version X.Y.Z` | choose the next tag (version mode `git-tag` only) |
| `graft version` | print the project version |
| `graft version bump [--minor\|--major]` | raise the version in the version file and its synced files without committing, for a script that folds the bump into its own commit (merging a work branch whose commits skipped it) |
| `graft version --describe` | the build identity for stamping binaries: `v1.4.0-3-gabc1234[-dirty]`, `1.4.0[-dirty]` in mode `file`, `v0.0.0-<commits>-g<hash>` without a tag |
| `graft init` | install the hooks and set `core.hooksPath`, create the `.env` file from `dotenv_template` when it is missing, install pinned tools that fail their check; safe to repeat |
| `graft check` | run the content checks on the index |
| `graft leaks <revision>…` | scan commits for the private names and terms of the user config (`leaks`) |
| `graft gate` | run the gate commands (`go vet ./...` in every Go module first, see `go_vet`, then `go fix -diff ./...` with `go_fix`, then `gofmt -l` with `go_fmt`) |
| `graft hook pre-commit` / `prepare-commit-msg` / `post-merge` | what the installed hooks call |
| `graft flags [status]` | is the flag raised: open journal events no exception mutes |
| `graft flags show <id>` | one event in full; `<id>` is its id or a prefix only one open event has; a closed event takes its full UUID |
| `graft flags ack <id> --reason R` | close an open event with a reason |
| `graft flags mute <id> --reason R [--match S] [--in subject\|body\|key] [--all-envs]` | add an exception for the event, then close it if it is open |
| `graft deploy [--redeploy] [--unapproved] [--past-freeze REASON] [--glint-ticket KEY] <target> [-- args]` | run the project's deploy script between graft's checks; `--redeploy` deploys the commit the target already runs, `--unapproved` skips the `approved_release` check, `--past-freeze` records a reason for deploying through an active freeze, `--glint-ticket` names the item that takes the glint findings of the deployed commit (`release_notes.glint`) |
| `graft deploy freeze <target> [Jira version]` | cut and push the configured freeze branch and annotated freeze tag; without a version, show the active freeze |
| `graft deploy thaw <target> --reason REASON` | end the active freeze and remove its freeze and journal tags from every remote |
| `graft deploy check <target>` | what `graft deploy <target>` would refuse now, read only: unpublished work, the window, `requires`, the hold, `approved_release` with the items of the commits outside it (summary, status, fix versions); exit 1 when it would refuse |
| `graft deploy versions` | the version and the commit each target runs, with the remote branches at it, required targets first |
| `graft deploy cutoff <target>` | the release candidate of the target's window: its unchecked commits and the commands that revert them on a release branch |
| `graft deploy status <target>` / `logs <target> [--lines N] [--grep RE]` | the target's status and logs commands |
| `graft deploy check-head` | for deploy scripts: HEAD is still the commit being deployed |
| `graft deploy check-integrations [build]` | for deploy scripts: the uploaded build configures every integration the target from `requires` runs (see Integrations) |
| `graft <task> [-- args]` / `graft run <task> [-- args]` | run a task with its deps |
| `graft tasks` | every task, building blocks and deps included |
| `graft start` / `stop` / `restart [service]` | background services, all of them without a name |
| `graft logs <service> [--lines N] [-f]` | the end of a service's log, or its journal; `-f` follows until Ctrl-C |
| `graft stats [--failures] [--since 7d] [--project <root>\|--all] [--task <name>]` | recorded resource use and recent failure reasons (see [Resource stats](#resource-stats)) |
| `graft exec [--heavy [--local] [--env <name>]... [--settle <duration>]] -- <command> [args]` | run a command that is no task: `--heavy` queues it for the machine as heavy tasks are, with `{threads}` (see Install, the user config), or sends it to the host of `remote_heavy` unless `--local`; the host gets no variable of the caller's but the go command's own unless `--env <name>` names it; `--settle` holds a separate FIFO lock for this long, waited on only by another `exec --settle` |
| `graft status` | services, the test database, held locks |
| `graft locks` | who holds graft's locks |
| `graft testdb up` / `down` / `status` / `recreate` | the disposable test PostgreSQL |
| `graft worktree add [--trunk <ref>] <branch> [<base>]` | a worktree under the user's cache, go.work's neighbours linked (see Worktrees) |
| `graft worktree clean [--apply] [--trunk <ref>] [--min-age 1h]` | remove the worktrees and branches whose work is in the trunk by content; list the rest |
| `graft worktree sync [--trunk <ref>]` | move this worktree's work onto the fresh trunk, uncommitted changes kept uncommitted (see Worktrees) |
| `graft tools [install]` | check the pinned tools; install the failing ones |
| `graft gocache trim [--older 25h] [--apply]` | remove the Go build cache entries unused for longer than `--older` (see [Go build cache](#go-build-cache)) |
| `graft help` | the commands the project's `.graft.yaml` uses, its tasks (those with `desc`, with their arguments) and services |
| `graft help <command>`, `graft <command> -h` | one command's part of `graft help`, with details such as the exit codes of `exec`; a command with flags lists them after it |
| `graft --version` | print graft's own version |

Put `-C <directory>` before a command to run it as if graft had been started
in that directory. Repeat `-C` to change directories in sequence; each relative
path starts from the directory selected by the previous one. This applies to
commands and tasks. `-C <directory>` after the command (before any `--`) is
refused with the form that works: `graft -C <directory> <command> …`.

### graft commit, step by step

1. Read the message (`-m`, repeatable, one paragraph each; `-F file`; `-F -`).
   An empty message is refused: there is no default subject. Warn when the
   hooks are not what `graft init` would install now: `core.hooksPath` not
   set to `hooks.dir` as written, or a shim missing or written by another
   graft.
2. Take the repository lock. A second graft in the same repository (or in
   another worktree of it) waits for the first, up to `lock.timeout`, and says
   who holds the lock. The lock is an OS file lock and dies with its process.
   With `lock.scope: worktree` each worktree has its own commit lock, so
   agents in separate worktrees commit in parallel; `graft release` still
   takes the repository lock, which the main worktree's commits share.
3. Refuse if a rebase, cherry-pick or revert is in progress, if HEAD is
   detached, or if there is nothing to commit. A merge in progress, a
   conflict or `git merge --no-commit`, is finished as a commit of its own
   by every step below, once `git add` has marked its conflicts resolved;
   files still in conflict are refused by name. With `ticket.required`, refuse
   a commit that names no work item, in the message or by the branch: only a
   revert and a commit whose files all match every target's
   `approved_release.ignore_paths` need none, and so does every commit on a
   branch in `ticket.skip_branches`. The ticket check uses the staged result
   after the gate, including files the gate writes.
4. Stage the work tree with `git add -A`, then run the gate. This makes new
   files visible to gate steps through `git ls-files`. If the gate fails, graft
   restores the index to its state before the command; work tree changes remain.
   After the gate, graft stages the paths that were changed when it started,
   as the gate left them. For an amend, this includes every path changed by
   the amended commit, measured against its parent. Paths first changed while
   the gate ran stay out of the commit so another writer's work is not committed
   before it was gated.
5. Bump the version file and every file synced with it, except on a branch
   in `version.skip_branches` (`--minor` and `--major` are refused there).
   On a branch in `version.build_branches` the bump counts builds in a fourth
   part instead: 1.2.3 -> 1.2.3.1 -> 1.2.3.2, a number the trunk never takes
   (`--minor` and `--major` are refused there too). A fourth part anywhere
   else, left by a merge from such a branch, is refused: put back the version
   the branch had.
6. Stage the version bump paths, then check the staged changes (secrets,
   binaries, sizes, version drift, and in a public repository private names in
   the added lines and the message). A failure restores the version files and stops.
7. Commit. The message is kept verbatim (`--cleanup=verbatim`): `$`, quotes,
   backticks and lines starting with `#` survive. When the branch name contains
   a work item key (`ticket.pattern`) and the message does not mention it yet,
   the key is added as a paragraph of its own, before a closing trailer block
   (`Co-Authored-By: ...`) so git still sees the trailers; a branch in
   `ticket.skip_branches` lends no key. The only trailers graft adds are
   `Worker` ones (see [Worker trailers](#worker-trailers)).
8. Push to each remote in turn, with `--force-with-lease` on a branch in
   `push.force_with_lease`. A failing remote does not stop the others; the
   summary names every failure with the command to retry, and the exit status
   is non-zero.

### Hooks

`graft init` writes small `sh` shims into `hooks.dir` (`.githooks` by
default) and points `core.hooksPath` at it with a relative path. Commit the
shims; every clone runs `graft init` once. The shim exits with an error and an
install hint when `graft` is not on `PATH`, so a missing binary never lets a
commit through unchecked.

- **pre-commit**: lets graft's own commits through (graft already ran
  everything). For a commit git's machinery drives (merge, rebase, cherry-pick,
  revert) it runs the checks and the gate. A hand-written `git commit` is
  refused with the graft commands this project's config allows, except on a
  branch of the user config's `commit.local_branches`, where it runs the
  checks alone.
- **prepare-commit-msg**: adds the `Worker` trailers to a message git
  commits itself (`git commit`, a merge, `git merge --squash`).
- **post-merge**: after a real merge commit (one with a second parent) it runs
  the gate. A merge without text conflicts can still break the build; the hook
  says so and points at `graft amend`. Fast-forwards are not gated.

`graft init` rewrites shims an older graft wrote. It refuses to overwrite hooks it did not
write and to repoint a
`core.hooksPath` that leads elsewhere. An absolute `core.hooksPath` to the same
directory is rewritten as relative. On Windows, keep the shims LF:
add `.githooks/* text eol=lf` to `.gitattributes`.

### Worker trailers

A tool that runs a coding agent on a job leaves a marker while the job
lives: the file `<git common dir>/worker/<id>`, a JSON object

```json
{"id": "job1", "models": ["vendor/model-a"], "client": "cli-a",
 "dir": "/abs/path/of/the/worktree", "pid": 12345, "finished": false,
 "started": "2026-01-02T03:04:05Z"}
```

written under a name starting with a dot and renamed (such names are skipped),
and removed when the job is over. `models` lists the exact model ids, in
order; `pid` is the process that lives as long as the job's round runs, its
command line naming the `id`; `finished: true` marks a round that ended as it
should, its work not yet taken: the marker stays until then, the process gone.
`graft commit`, `graft amend` and the `prepare-commit-msg` hook add a
`Worker: <model>` trailer for each model whose work the commit carries, once,
and not when the message already names it:

- a marker whose `dir` is this worktree and whose round is finished or
  whose process lives: the job's work is here;
- a marker of another worktree whose round is finished, when the commit
  stages some file byte for byte as the job left it there, committed or not,
  and the job changed that file since its worktree forked from `HEAD`: its
  changes were carried over by hand (`git diff` there, `git apply` here),
  the master's own edits on top of some files aside;
- the `Worker` trailers of the commits a merge (`MERGE_HEAD`) or
  `git merge --squash` brings in.

Project version files and files named `go.mod`, `go.sum`, `go.work` or
`go.work.sum` do not count as carried work. The marker of another worktree
whose job still runs names no model, nor does one whose files the commit
changes to other text, or puts back as they were
before the fork: two jobs on the same code do that for their own reasons. The
commit says how many files it carries of which job. A marker is a hint and
never stops a commit: one that cannot be read, does not parse or names no id,
model, dir or one of pid and finished, one whose worktree is gone or cannot be
read, and one of a round not finished whose process is gone (the job was
killed and left it) is reported and passed over. On Linux a pid whose
command line does not name the `id` counts as gone: another program took it.
Without markers the message is left as it is.

## .graft.yaml

```yaml
schema: 1                       # required
graft: '>=0.5.0'                # optional: the oldest graft this config works with

# Commands run before every commit, in order, stopping at the first failure.
# A string is split into words by shell quoting rules but is NOT run by a
# shell: pipes, &&, redirects, $VAR, globs and VAR=value prefixes are
# rejected. Use the list form for exact argv, or call a script.
gate:
  - task: smoke                 # a task from the tasks section
  - [go, test, -short, ./...]
  - sh: go vet ./... && go test ./...   # run by sh -c

# The gate starts with go vet ./... of its own, before the steps above: vet
# analyzers such as composites and copylocks catch what linters do not. It
# runs in the root when it has go.mod and in every directory below it with a
# go.mod that git lists (tracked, or untracked and not ignored), at any depth,
# except paths with a testdata, vendor or node_modules segment; a module below
# the root shows as "go vet ./... (backend)". With a go.work in the root only
# the modules of its use are vetted: a go.mod outside it is not part of the
# build. A failing vet fails the gate like
# any step. Turning it off needs a reason. A project that builds with flags
# of its own (GOFLAGS=-tags=...) gives them in go_vet.env: vet with other
# flags is another build, which neither shares the build cache nor sees the
# same files. A vet step of the project's own gate then only repeats it.
go_vet:
  enabled: true                 # default true; false turns all vet steps off
  # env: {GOFLAGS: -tags=foo}   # variables of the vet steps, as a task's env
  # exclude: [tools/gen]        # module directories left unvetted (with everything below)
  # reason: ...                 # required with enabled: false or exclude

# gofmt -l checks every Go file git lists inside the modules, after go vet and
# go fix. It skips testdata, vendor, node_modules and go_vet.exclude. Files
# with different formatting or parse errors fail the gate; CRLF is treated as
# LF. It is on by default so unformatted Go files do not enter a commit.
go_fmt:
  enabled: true                 # default true; false turns the check off
  # reason: ...                 # required with enabled: false

# go fix -diff ./... after the vet steps, in the same modules with go_vet.env,
# shown as "go fix -diff ./... (backend)" below the root: a rewrite go fix
# would make fails the gate with its diff. Run go fix ./... in the module
# until go fix -diff ./... is empty (one pass skips rewrites that overlap)
# and commit them. Off by default: a new Go release brings new rewrites, and
# after a Go upgrade the step fails in every project that turns it on at
# once, whatever the change; the failure says so.
go_fix:
  enabled: false                # default false

version:
  mode: file                    # required: file | git-tag | none
  file: VERSION                 # file mode; default VERSION, MAJOR.MINOR.PATCH (.BUILD on build branches)
  tag_prefix: v                 # default v
  tag_on_commit: false          # file mode: tag every commit v<version> and push the tag
  skip_branches: ['w/*']        # file mode: commits on these branches leave the version alone;
                                # the branch they are merged into bumps (graft version bump)
  build_branches: ['hotfix/*', 'release/*']  # file mode: commits here bump a fourth part, 1.2.3.1
  sync:                         # file mode: files that repeat the version
    - path: web/package.json
      format: json              # value at the key path; formatting is preserved
      key: [version]
    - path: web/package-lock.json   # npm keeps the version twice in the lock:
      format: json                   # several targets in one file are applied
      key: [version]                 # together and the file is written once
    - path: web/package-lock.json
      format: json
      key: [packages, "", version]
    - path: config.yaml
      format: regex             # exactly one capture group: the version
      pattern: 'version:\s*"([^"]*)"'
    - path: docs/VERSION
      format: plain             # the whole file is the version

ticket:                         # omit to never add a key
  pattern: '[A-Z][A-Z]+-[0-9]+' # first match in the branch name
  required: false               # true: a commit must name a work item
  skip_branches: ['agent/*']    # commits on these branches need no work item and take no key from the branch

push:
  remotes: []                   # empty: every configured remote
  force_with_lease: ['w/*']     # branches rebased after a push: pushed with --force-with-lease

checks:
  secrets:
    enabled: true               # default true
    extra_patterns: []          # more regexes, matched line by line
    exceptions:
      - path: 'testdata/**'
        reason: synthetic keys for tests   # a reason is required
  large_files:
    enabled: true               # default true
    max_binary_bytes: 1048576   # default 1 MiB
    max_text_bytes: 5242880     # default 5 MiB
    binary_extensions: [png, jpg, jpeg, gif, webp, ico, svgz, woff, woff2, ttf, otf, eot, pdf, mp4, webm, wasm]
    exceptions:
      - path: docs/vendor/diagram.min.js
        reason: vendored library, updated on purpose
  reverts:
    enabled: true               # default true
    depth: 60                   # default: recent commits of HEAD's first-parent line
    exceptions:
      - path: 'testdata/golden/**'
        reason: regenerated, goes back and forth

hooks:
  dir: .githooks                # default .githooks

lock:
  timeout: 15m                  # default 15m
  scope: repository             # repository (default) | worktree: one commit lock per worktree;
                                # not with version.tag_on_commit
```

Unknown keys are errors, so a typo cannot silently switch a check off. All
paths are relative to the repository root and use forward slashes.
Exception paths are globs: `*` and `?` stay within one directory, `**` spans
directories.

## Tasks

Tasks replace Makefile targets. `graft smoke` runs the task `smoke` (a task
cannot take the name of a graft command); `graft help` lists tasks that have
a `desc`, the rest are building blocks.

```yaml
tasks:
  build:
    desc: build the binary
    run:
      - go build -o bin/app ./cmd/app          # argv, no shell
  test:
    desc: all tests
    deps: [build]              # run first, each task at most once per invocation
    test_db: true              # pass the test database DSN (see below)
    heavy: true                # wait while the machine is loaded (user config load), GRAFT_THREADS set
    remote: false              # heavy: stay on this machine though the project has remote_heavy (true: any step goes, and a busy gate sends its go steps)
    tests: go                  # read go test -json: print plain go test output, record every test
    dotenv: [API_KEY!, 'LOG_DIR?']  # only these .env keys reach the steps; KEY? optional, KEY! must not be empty
    dotenv_sets: [db]          # plus the keys of named sets
    env: {LOG_LEVEL: debug, API_URL: 'http://${HOST}:8080'}   # ${KEY}: environment, then .env
    run:
      - go test -json -p {threads} -count=1 {args} ./...   # graft test -- -run TestX; {threads}: user config load.threads
  lint:
    keep_going: true           # run every step, fail at the end listing what failed
    lock: read                 # shared lock "work-tree": lints run side by side
    run:
      - go vet ./...
      - sh: gofmt -l . | tee fmt.txt       # a shell step, when a pipe is the point
      - task: build                        # another task, inline
      - service: restart api               # start|stop|restart [service], all services without a name
  restore:
    desc: load a dump
    args: required             # refuse to run without arguments
    usage: <dump.sql.gz>       # shown in help and in that refusal
    run:
      - [sh, scripts/restore.sh, '--from={args}']   # inside a word: exactly one argument
  release-build:
    run:
      - [go, build, -ldflags, '-X main.version=${GRAFT_VERSION}', -o, bin/app, ./cmd/app]
  migrate:
    lock: {name: db, mode: write}          # exclusive: waits for readers, blocks them
    dir: migrations                        # steps run here; default the repository root
    run: [[sh, apply.sh]]

cache_exclude: [docs/**]       # paths the task cache does not key on

dotenv_sets:                   # key lists several tasks and services share
  db: [DB_HOST, DB_NAME, 'DB_POOL?']
```

- A step is a string (split into words, no shell; pipes, `$VAR` and
  redirects are errors), a list (argv as is), `{sh: "..."}`, `{task: name}`,
  `{service: action [name]}` or `{install: paths}` (below). A string with
  `: ` in it is a map to YAML: quote the whole step.
- `{install: paths}` puts built files where programs live: the paths, globs
  among them, are relative to the task's `dir`, and a glob that matches
  nothing is an error. They go to `tools.bin_dir` of the user config, or
  where `go install` puts binaries; `{install: paths, to: dir}` names
  another directory, absolute or starting with `~/`. A file is installed
  only from what is committed: a Go program's build info must say HEAD and
  no uncommitted changes (so a stale build in `bin/` is refused too), and a
  file without it needs a clean work tree. A Go binary without
  `vcs.revision` built in a linked worktree is refused because `go build`
  does not embed VCS metadata there; install it from the main worktree after
  merging. The copy is atomic and
  executable, a running program keeps its old file, an identical file is
  left alone (a symlink there, say to the build output, is replaced with a
  file), and each file prints `graft: install: <name> <revision> ->
  <path>`. With `install.hosts` in the user config, the same file is sent by
  `rsync` to each host at that absolute path, creating its directory; a failed
  host fails the step after the local install. A task with an install step is
  never cached. The usual install
  of a project's program:

  ```yaml
  tasks:
    build:
      run: [[go, build, -o, bin/tool, ./cmd/tool]]
    install:
      desc: install bin/tool into ~/bin
      deps: [build]
      run: [{install: bin/tool}]
  ```

  Steps a project needs around it (a check that nothing runs the old
  program, a config file) stay `sh` steps of the same task.
- `{args}` receives the arguments after `--` verbatim: as separate words
  when it is a whole argv word, as the one argument inside a word, and
  shell-quoted in `sh` steps. `${KEY}` is expanded in the configured words
  only, never in what the user passed. A task without `{args}` refuses
  arguments; `graft help` shows which tasks take them. Inside `[...]`,
  quote it: `'{args}'`, `'--run={args}'`. Unquoted, YAML reads `{args}` as a
  map and cannot parse `--run={args}`; graft says so.
- Steps get graft's environment without git's hook variables, without
  `PWD` (the caller's directory, which the go command would hash into the
  key of a test that calls `os.Getwd`: the same code called from elsewhere
  would miss the test cache; a shell sets its own), and without
  the `.env` file: only the keys listed in `dotenv` and `dotenv_sets`, the
  `env` values, the test database DSN and `GRAFT_VERSION` are added.
  `GRAFT_VERSION` is `graft version --describe`; before the first commit
  there is none. In the gate of `graft commit` and `graft amend`,
  `GRAFT_COMMIT_MESSAGE_FILE` names a file with the message of the commit
  being made (the final text, work item key included), for checks that hold
  the message to what the change does; it is removed after the gate. Argv
  words see all of them as `${KEY}`.
- Each work tree gets a golangci-lint cache of its own:
  `GOLANGCI_LINT_CACHE` is set to a directory under the user cache
  (`~/.cache/graft/golangci-lint/<hash of the tree's path>`) unless the
  environment or the task's `env` sets it. golangci-lint keys a package's
  results by content but keeps the paths of the tree that produced them, so a
  shared cache hands one worktree another's findings and reads their
  `//nolint` there: once that tree is changed or removed, suppressed findings
  fail the lint ([golangci-lint#6695](https://github.com/golangci/golangci-lint/issues/6695)).
  A new worktree's first lint runs cold; where the directory cannot be made,
  graft says so once and the steps share golangci-lint's own cache. A value
  graft set itself, inherited by a graft in another work tree, is replaced.
  `graft worktree clean --apply` removes the caches of work trees that are
  gone.
- `go build`, `go install`, `go run` and `go test`, as a step's argv or a
  `graft exec` command, get `-trimpath` after the subcommand unless their
  flags or `GOFLAGS` name it already, `-trimpath=false` included: without it
  the go command keys each package of the module by its directory, and every
  worktree of a repository fills the build cache with its own copy of what
  the others built. A test that finds files through `runtime.Caller` gets a
  path from the module root under it, not one on disk: take paths from the
  working directory, the package's own, instead; a step that cannot
  sets `GOFLAGS: -trimpath=false`. Under `-trimpath` a program or test takes
  `runtime.GOROOT()` from its environment only, so `go run` and `go test`
  given it get `GOROOT` from `go env GOROOT`, run in the step's directory,
  unless their environment sets it: `go/importer` and `go/build` find the
  standard library through it. On the host of `remote_heavy`, which does
  not get this machine's environment, `go run` and `go test` go without
  `-trimpath`. A binary `go build` or `go install` made
  reports no `GOROOT` where it runs unless the environment sets it.
  `go vet` and `go fix` go without, and
  `GOFLAGS` is left alone for the commands steps start themselves: under
  `-trimpath` both replay a result another worktree cached with that
  tree's paths, and `go fix` rewrites the files there.
- Before the first step, deps included, graft fits the arguments into the
  steps and checks every required key and `${KEY}` the task and all its deps
  need, listing everything missing at once. An optional `KEY?` set nowhere is
  left out; a `KEY!` set but empty is an error. Inside `[...]` it must be quoted:
  YAML does not take `KEY?` unquoted in a flow list.
- Locks are named read/write locks shared by all worktrees of the
  repository; a task inside a task under the same lock reuses it, and asking
  for write inside read is an error. Unknown deps and cycles are config
  errors.
- `tests: go` reads the steps' stdout as a `go test -json` stream; the step
  passes `-json` itself, graft adds nothing to the command. The terminal
  shows what plain `go test` would: package lines, build errors, benchmark
  results and the output of failing tests; with `-v` in the command, every
  line. Lines that are not events pass through. Unlike plain `go test` it
  names the skipped tests, subtests included, under their package line
  with the message of their t.Skip as the reason, not earlier logs nor the
  stderr of parallel tests go test hands them (`skipped 2 tests in m/a:`, then
  `    TestCorpus: a_test.go:9: no corpus`), a cached package's too, and
  counts them all in a last line (`skipped 3 tests in 2 packages`): a test
  skipped for want of a file or a service looks green otherwise. Every package run is
  recorded in the tests file (see below). A step with `go test` in it that
  prints no events gets a warning: `-json` is missing. Packages after `--`
  replace the ones a `go test … {args} ./...` step names itself, with a line
  saying so: `graft test -- -run TestX ./cmd/x` tests `./cmd/x` alone, while
  flags alone keep `./...`.
- `cache: tree` skips a task that already passed on the same inputs, its
  deps and inner tasks with it: `graft: lint: passed on this tree
  2026-10-16 10:12, skipped`. The key covers the work tree by content (every
  file `git add -A` would commit; a rebase or a commit of the same changes
  keeps it), less `cache_exclude` and the version files graft commit bumps;
  the definition of the task, its deps and inner tasks; the values their
  `env` and `.env` keys take; the arguments; this graft's version and
  platform; and, in the gate, the commit message when one of them names
  `GRAFT_COMMIT_MESSAGE_FILE`. `GRAFT_VERSION` is left out: it changes with
  every commit. What else the result depends on goes in a map:

  ```yaml
  lint:
    cache:
      paths: ['**/*.go', .golangci.yml]    # work tree files this task reads
      env: [GOFLAGS]                       # values of these variables
      files: ['~/.config/linter.yaml']     # content of files outside the tree or ignored
      commands: [[go, version], [docker, image, inspect, -f, '{{.Id}}', 'postgres:18']]   # their stdout
    run: [golangci-lint run]
  ```

  `paths` narrows the work tree files in the key to the union of this task's
  paths and the paths of its deps and inner tasks. Every dep and inner task
  must declare `cache.paths`; otherwise graft cannot narrow the key for its
  input, which may depend on the whole tree. Include every file the check reads
  outside those paths, such as source files, includes or a linter config in the
  tree, or a change to that file will not rerun the task.

  Only passes are kept, under `<user cache dir>/graft/passed`, for 30 days,
  shared by every worktree and clone of the machine: one that holds the same
  content skips what another one passed. A pass is not kept when the tree
  or an input changed while the task ran. A command of `commands` that fails
  runs the task, and so does one that may not start in a sandbox (docker
  under no_new_privs, see the test database), which graft does not start.
  `--no-cache` on `graft <task>`, `graft commit` and `graft deploy`, or
  `GRAFT_NO_CACHE=1`, runs everything. A skip is recorded
  in the stats (see below) with what the passed run cost.

  Cache only checks that change nothing outside the work tree. A task that
  runs `graft deploy`, `release`, `commit`, `git push`, `docker push` or acts
  on services, itself or through its deps and inner tasks, is refused; what
  graft cannot see (a script that deploys) is the author's to keep out. A
  check whose result depends on something outside the tree that the key
  does not cover (a database state, a remote service) must name it in
  `env`, `files` or `commands`, or go without cache.
- `tests: jest` and `tests: playwright` read the JSON report the runner
  writes to the file named in `GRAFT_TEST_REPORT` (also `${GRAFT_TEST_REPORT}`
  in argv and task `env` values); the terminal shows the runner's own output.
  A test file is a package; a jest test is its full name, a playwright one
  `[project] describe › title` with the time of all its attempts. A test a
  retry passed within the run is recorded as `flaky`. Playwright errors with
  a file location fail that file's package; errors without a file location,
  such as configuration errors, fail the `.` package. Each step of a task
  configured with Jest or Playwright gets its own report path, task `env`
  included. A step that names the runner but writes no report gets a
  warning.

  ```yaml
  jest-unit:
    tests: jest
    run: [[npx, jest, --json, '--outputFile=${GRAFT_TEST_REPORT}']]
  e2e:
    tests: playwright
    run: [[env, 'PLAYWRIGHT_JSON_OUTPUT_FILE=${GRAFT_TEST_REPORT}', npx, playwright, test, '--reporter=dot,json']]
  ```

### Resource stats

Every process a step starts (task steps, the gate, a deploy's deps and the
deploy script) is recorded when it ends: one JSON line in
`<user cache dir>/graft/stats.jsonl` (`$XDG_CACHE_HOME/graft` where set, else
`~/.cache/graft`), shared by every project and every graft on the machine.
A task gets a line of its own when it ends, covering everything it ran,
deps and inner tasks included.

```json
{"time":"2026-01-05T10:00:03.1+01:00","kind":"step","project":"/home/me/work/projectA","profile":"projectA:lint","task":"lint","top":"commit","step":"shellcheck scripts/*.sh","wall_s":41.2,"user_s":38.9,"sys_s":2.4,"max_rss_mb":9123.4,"tree_mem_mb":11840.2,"read_mb":12.5,"write_mb":310.2,"peak_write_mbs":96.4,"load_start":4.12,"load_max":9.8,"exit":0}
{"time":"2026-01-05T10:00:03.1+01:00","kind":"task","project":"/home/me/work/projectA","profile":"projectA:lint","task":"lint","top":"commit","steps":2,"wall_s":44,"user_s":40.1,"sys_s":2.6,"max_rss_mb":9123.4,"tree_mem_mb":11840.2,"read_mb":12.6,"write_mb":310.4,"peak_write_mbs":96.4,"load_start":3.9,"load_max":9.8,"exit":0}
```

- `profile` is the kind of run, for telling what a run of the same kind
  cost before: `<repository>:<task>`, the repository named by the folder of
  its main work tree, so every worktree of it shares the profile, and for
  a `graft exec` `<repository>:exec:<program>`, the program its command
  starts with. Outside a repository, the folder graft ran in names it.
- `top` is the graft command the task ran under (`commit`, `amend`, `gate`,
  `hook`, `deploy`, `start`), or the outermost task for `graft <task>`. A
  deploy is recorded as task `deploy <target>`: its deps, the `sudo`
  confirmation (a wait) and the script, which is its step.
- `in`, on a task line, is the task or deploy it ran in, as a dep or a
  step, in the same graft; absent for the outermost.
- `run` names the graft process that wrote the line, `parent` the graft
  that started it: graft passes its run to every process it starts in
  `GRAFT_RUN_ID`, so a `graft <task>` in a step or a deploy script records
  the outer graft as its parent. Lines of a graft started by no graft have
  no `parent`.
- `wait_s`, on a task line, is the part of `wall_s` the task and the tasks
  it ran waited before their steps: for the load in the queue of heavy
  tasks and for their locks. Absent when nothing waited.
- `wait_why` splits `wait_s` by what was waited for, in seconds: `queue`
  (behind other heavy tasks), `load`, `io`, `swap` (each over its limit;
  the time between two readings goes to what the first found), `quiet` (the
  quiet mode of akit's busy), a measure graft does not know yet by its own
  name, `settle`
  (the hold after a wait), `lock` (a task lock) and `sudo` (a deploy's
  confirmation). Absent when nothing waited.
- `waited_behind`, on a task line that waited for a queue or lock, names the
  command and task ahead of it (for example `graft build (starting a heavy
  task)`). Queue time overlapping quiet mode is counted as `quiet`, not
  `queue`.
- `host`, on the lines of a task run on the host of `remote_heavy` and of
  its steps, names that host; absent for this machine. Their CPU time,
  memory and disk IO are those of the command that sent the step there, not
  of the work.
- `here`, on the line of a heavy task that had a `remote_heavy` host and
  ran on this machine, is why: the reason its `heavy, on this machine:`
  line gave, the broken path, or, where graft says nothing, `this machine
  would not make it wait` (`when: busy` on a calm machine) and `inside an
  admitted heavy task, which waits for the machine`. Absent for a task the
  host ran and without a host.
- `disk_profile`, on the line of a heavy task this machine admitted, is the
  profile the disk state source gave its `profile` at the admission's last
  reading, as the source wrote it; absent without the source.
- `step` is the step as written in `.graft.yaml`, on one line, cut to 200
  characters. `exit` is -1 when there is no exit status: killed by a signal,
  or a task that failed outside its processes.
- `max_rss_mb` is the peak memory of **one** process: the step's own or that
  of a descendant it waited for, whichever is largest (`ru_maxrss` of
  `wait4`). It is not the sum over the process tree: eight linters in
  parallel at 1 GB each show 1 GB. Processes that are never waited for
  (daemons, detached children) are not counted, in memory or in CPU time.
  Windows reports no peak memory; the field is left out there.
- `tree_mem_mb` is the peak of the memory the whole process tree held at
  once: the step's process and every descendant, summed, so four test
  binaries of 1 GB running side by side show 4 GB. graft reads it from
  `/proc` every 200 ms while the step runs: per process `RssAnon` plus
  `RssShmem` of `/proc/<pid>/status`, the memory that is not backed by
  files. Binaries, libraries and page cache are left out: processes share
  them and the kernel takes them back under pressure. A process that lives
  less than the interval may be missed; shared memory two processes map is
  counted in each. Processes that leave the tree (daemons, containers
  started through a docker daemon) are not counted. A task line has its
  largest step's. Linux only; elsewhere the field is left out.
- `read_mb` and `write_mb` are what the step and the descendants it waited
  for read from and wrote to the storage (`ru_inblock` and `ru_oublock` of
  `wait4`); a task line has its steps' sum. A write counts when it dirties
  the page, before the kernel writes it out; reads from the page cache and
  writes to tmpfs count nothing. Linux only.
- `peak_write_mbs` is the most the process tree wrote in a second, MB/s:
  graft sums `write_bytes` of `/proc/<pid>/io` over the tree every 200 ms,
  read with `tree_mem_mb`, and takes the fastest growth over a stretch of a
  second or more. A parent's `write_bytes` takes in those of the children
  it reaps; what a child gone from the tree was counted for comes off the
  growth of its nearest ancestor still there over the next two readings
  after it is gone.
  What graft may not read (a process under `sudo`, while it is so) is left
  out, and so is that ancestor's growth over the two readings after such a
  process is gone; a step whose tree graft cannot read at all, and a step
  shorter than a second, have no peak. Writes in the last 200 ms of a step
  are not read. A task line has its largest step's. Linux only.
- `load_start` and `load_max` are the machine's 1-minute load average when
  the step or task started and the highest seen while it ran, read with
  `tree_mem_mb`. Wall time on a machine at load 30 does not compare with
  wall time at load 5: they tell such runs apart. Absent where there is no
  `/proc/loadavg`.
- `db_cpu_s` and `db_mem_mb`, on the line of a task with `test_db: true`,
  are what PostgreSQL in the test database's container cost, which the
  task's own processes do not show: the CPU time of the container's cgroup
  from the task's first step to its end, and the highest `memory.current`
  read every 200 ms, page cache and tmpfs data included. Bringing the
  container up and `migrate` come before and are not counted. The container
  is shared: tests run at the same time from another worktree or session
  against it count in this task's numbers too. Only when graft runs the
  container (not with a DSN from the environment) and sees its cgroup v2
  from the host: Linux with docker on the host itself, not Docker Desktop.
- A line is one append-mode write, so concurrent grafts never split each
  other's lines. At 20 MB the file moves to `stats.jsonl.1` and the older
  copies one number up (`.2`, `.3`…, the highest the oldest); a copy whose
  last line is more than 15 days old is removed then, so two weekly windows
  can be compared; one that cannot be removed is warned about, and the line
  still written. `graft stats` reads the copies that reach `--since` and the
  oldest, and when the log starts after `--since`, its header names the
  first line instead.
- A failure to record is not a failure of the task: graft warns once per run
  and goes on.

A task with `tests:` also writes `tests.jsonl` next to it: one line per
package run, the test binary's time without the build, the top-level tests
as `[name, wall_s, result]`, and the subtests that failed or were skipped.
A fourth field holds a skipped test's reason or the first lines of a failed
test's output; package `reason` holds build output or package-level failure
output when no test failed. Reasons have at most 10 lines, 300 characters per
line and 2000 characters total, with terminal color codes removed. Cached
packages did not run and are left out.

```json
{"time":"2026-01-05T10:00:03.1+01:00","project":"/home/me/work/projectA","task":"test","top":"commit","tree":"9f2c...","runner":"go","package":"example.com/a/store","result":"fail","wall_s":2.7,"tests":[["TestSave",2.5,"pass"],["TestLoad",0.1,"fail","load_test.go:18: database unavailable"],["TestTable/empty",0,"fail","want empty result"],["TestTable",0,"fail"]]}
```

- `tree` identifies what the work tree held: HEAD, the changes to tracked
  files and the untracked files that are not ignored. Two runs with the same
  tree ran the same code.
- `runner` is the report the line was read from: `go`, `jest` or
  `playwright`. For jest and playwright a package is a test file; its time
  is the file's run for jest and the sum of its tests for playwright, whose
  files run spread over workers.
- `reason` on a failing package holds up to 10 lines of build output or
  package-level failure output. A failing test's fourth field is its output,
  without Go test framing lines.
- `load_start` and `load_max` are those of the step, as on its stats line.
- `result` is `pass`, `fail`, `skip`, `build-fail` or, for a test a retry
  passed within the run, `flaky`: it counts as failed and passed on its
  tree. A package cut off (a timeout, a killed process) fails with the tests
  that were running.
- A test takes about 45 bytes, so a suite of 2000 tests writes 90 KB a
  run. The file moves to `tests.jsonl.1` at 64 MB, its copies kept as those
  of `stats.jsonl` are: at least 15 days, and 700 runs of such a suite a
  copy.

A failing `step` or `task` line in `stats.jsonl` has a `reason` with up to
10 lines of stderr's tail, or stdout's tail when stderr was empty; for
`tests: go`, it uses the parsed failed package or test reasons, falling back
to stderr when they are empty. A task line uses the first failing step's reason.
Output sent directly to a terminal is not captured, so its failing step has
no `reason`; this keeps the child connected to the terminal.

`graft stats --failures` lists recent failed steps and task failures that
happened before a step ran, newest first. Each row shows the time, project
tree, task, step, exit code and the complete recorded reason. `--since` sets
the window; `--task` selects a task, `--project` selects a project tree, and
`--all` includes every project. Use `--in <task>` with `--task` to select
failures from runs in that task. A red gate points to this report. When a task
fails with the same first reason as its previous failed run in the same tree,
graft prints how long ago that reason appeared and the consecutive failure
number. Successful runs break the streak. Captured reasons omit graft status
lines and warnings.

A task the cache skipped is a `"kind":"cache-hit"` line with the wall and
CPU time of the run whose pass it reused: the time the skip saved.

A task cache miss is recorded as `"kind":"cache-miss"` when an earlier pass
of that task exists in the same work tree. `cache_changed` lists the parts
that differ from that pass: `tree`, `definition`, `inputs` (environment,
`.env`, arguments and declared cache inputs), or `graft`. The cache stores a
hash of each part alongside the pass; input values themselves are not written
to stats.

`graft stats` reports the current project for the last 7 days, one row per
task, the heaviest in memory first: runs, median and maximum wall time,
when some run waited the median and maximum wait within it (`WAIT MED`,
`WAIT MAX`) and what the runs waited for, the longest first, with its total
(`WAIT WHY`, from `wait_why`), median CPU time (user + system), the largest peak of the process tree
(`TREE MEM`) and of a single process (`MEM MAX`), the share of failed
runs, and the wall time the cache saved (`SAVED`, with the number of
skips); a last line sums the skips, wall and CPU time. When tasks recorded
their test database, `DB CPU` and `DB MEM` follow: its median CPU time and
largest memory. `--since` takes days (`30d`) or a duration (`12h`), `--project` another
project's root, `--all` every project; `--task <name>` breaks one task down
into its steps, the tasks it ran and the grafts it started: a dep or a
task step is a row `task <name>`, and a step or deploy script that runs
`graft <task>` shows that graft's outermost task as a row `graft <task>`,
their wall time part of the task's own.

Under the table a line `over the limit: <task> takes 1m12s by median without
waits, above 1m0s (user config limits.gate)` marks the gate over
`limits.gate` and any other task over `limits.task`: the median of the
period's runs, the waits for the load and the locks taken out; the 10
farthest over show, the rest counted. `--max-load` leaves the runs on a busy
machine out of it too.

A run says so itself as it ends: when graft's outermost task (the gate of
`graft commit`, `graft <task>`, `graft exec`) took longer than its limit
without its waits, graft prints `graft: over the limit: merge took 9m12s
without waits, above 8m0s (user config limits.task)` after it, failed or
not. Inner tasks and deps count in the outermost.

`graft stats --tests` reports the tests file instead: the 20 slowest
top-level tests and packages by median wall time, with their runs, maximum
and share of failed runs, the 20 most skipped tests (skipped ones are left
out of the timing) with how many runs skipped them, the runs of their
package and the reason of the last skip, and the flaky tests: those that both failed and
passed on the same tree, with how many trees saw that. A test that fails
after a change and passes after the next one is not flaky; the same code
failing and passing is. The flaky tasks follow: a task that failed and then
passed under the same graft, as when a test script run by a task reruns a
failed stage `graft e2e`, is a retry that hides a flake the way a test
runner's retry does; the table counts the runs of the outer graft that did
it and the failed attempts. Two runs by hand are not a rerun, and lines
written before graft 2.19 carry no run to tell. `--since`, `--project`,
`--all` and `--task` select the runs as above.

`--max-load 16`, in both reports, counts only the runs whose `load_max`
stayed below 16; task load includes waits before steps, and runs with an
incomplete load sample are left out with runs recorded without a load. Pass
a positive value when setting `--max-load`; zero and non-finite values are
usage errors.

For a nightly speed check, `graft stats --value wall-p50 --task build --since 24h`
prints one number of seconds, with at most three decimal places.
The wall values are a run's own time, `wall_s - wait_s`: its wall time without
the wait for the machine. `wall-p95` gives the nearest-rank 95th percentile, and
`wait-p95` gives the
95th percentile of waits. Wall values use successful runs; `wait-p95` without
`--task` uses outer task runs. The default minimum is three runs. Fewer runs
prints no value, explains the count on stderr and exits with code 3; set
`--min-runs` to change the threshold. The project counts the runs of all its trees: the main
checkout and its worktrees; `--all` counts every project.

Use `--in <task>` with `--task <name>` to select runs of that task whose
`in` field names the task or deploy that ran it. For example,
`graft stats --task lint --in smoke` reports only `lint` runs in `smoke`;
`--value wall-p50` and `--value wall-p95` use the same selection.
`--in` requires `--task` and cannot be used with `--tests` or `--value wait-p95`; the report keeps the steps, inner tasks and grafts of the selected runs, matched by the graft process that ran them (a process that ran the task in two parents shows the steps of both).

## Services

Long-running local processes started in the background, the native
counterpart of a dev container.

```yaml
services:
  api:
    desc: the API server
    build: build                 # a task run before start
    run: [bin/app, serve]
    dotenv: all                  # the whole .env, or a list of keys
    env: {LOG_FORMAT: json}
    addr: ':${API_PORT}'         # ${KEY} from the environment or .env; start waits until it accepts connections, stop until it is free
    pidfile: tmp/api.pid
    log: tmp/api.log             # appended to, one header line per start
    start_timeout: 30s           # default 30s
    stop_timeout: 10s            # default 10s, then SIGKILL
    lock: work-tree              # held for writing while starting or stopping
  worker:
    systemd_unit: worker.service # a systemd --user unit: start, stop, status and logs go through
                                 # systemctl and journalctl (daemon-reload first when the unit file
                                 # changed); run, pidfile, log, dotenv, env belong to the unit
    addr: ':9090'                # still waited for, if given
```

`graft start` refuses an address something else already listens on, waits
for the process to listen (without `addr`: to survive a second) and shows the
end of the log when it dies instead. `graft stop` sends SIGTERM to the
process group, so wrappers like `go run` do not leave the server behind, and
SIGKILL after `stop_timeout`. A pid file whose process is gone, or on Linux
now runs another program, is removed. On Windows stop terminates the process
itself, not its children: run the program, not a wrapper. `graft logs`
prints the end of the log file, or `journalctl --user -u` for a unit; with
`-f` it follows until Ctrl-C.

## Test database

```yaml
test_db:
  image: postgres:18-alpine
  container: app-test-pg
  port: 55432                    # published on 127.0.0.1 only
  # socket: true                 # instead of port: a unix socket, no network (Linux)
  database: app_test
  user: app
  password: app                  # a throwaway local database
  settings: [fsync=off, synchronous_commit=off, full_page_writes=off, jit=off]   # the default
  tmpfs: 2g                      # optional: keep the data in RAM (k, m or g; at least 64m)
  migrations: migrations          # optional: a source database per set of migration files
  migrate: [go, run, ./cmd/migrate, up]   # run after up with the DSN in dsn_var
  dsn_var: TEST_DB_DSN           # default
  ready_timeout: 60s             # default
```

A task with `test_db: true` gets the DSN in `dsn_var`. If that variable is
already set (a CI service container) graft uses it, after checking that it
names `database`; a DSN left over from real work never reaches the tests.
Otherwise graft starts the container (creating it if needed), waits for
`pg_isready`, runs `migrate` and, when `.env` exists, keeps `dsn_var` there
in step for tests started from an IDE. In `migrate`, `${<dsn_var>}` (say
`${TEST_DB_DSN}`) is the DSN of the database just brought up, whatever `.env`
holds; other `${KEY}` are looked up as usual. A container created with other
settings, another `tmpfs` or listening elsewhere (`port` or `socket`) gets
a warning; `graft testdb recreate` applies the config. `jit=off` is in the default settings since 2.15: a container
made earlier without `settings` warns until it is recreated. graft creates the container with `initdb --no-sync`
(`POSTGRES_INITDB_ARGS`): nothing in the cluster needs to survive a crash, and
a busy disk takes some 8 seconds to sync a fresh one, against 1.5 without.

With `migrations`, every set of migration files gets a database of its
own: `app_test_source` becomes `app_test_<hash>_source`, the hash of the
files' names and contents. Branches and worktrees share the container, and a
migration one of them applied no longer lands in the database another one
tests against (a migrate tool that finds a version it has no file for
refuses to run). graft creates the database on the first `up` of a set and
marks it used on every `up`; the database of another set unused for 7 days
is dropped, with the template and copies the testdb package made from it
(`app_test_<hash>_template`, `app_test_<hash>_*`). `database` itself stays:
it is the one the container starts with, and a DSN from the environment (a
CI service container) may still name it.

`socket: true` serves the database on a unix socket instead of a port. The
container runs without a network (`docker run --network none`): a published
port gives every container a network device on the host, a veth, created and
removed with it, which network managers and SELinux log each time. The
socket lives in `<user cache>/graft/testdb/<container>/` (bind-mounted with
`:z` for SELinux), and the DSN names that directory in its `host` parameter,
which libpq, pgx and lib/pq read:
`postgres://app:app@/app_test?host=%2Fhome%2Fu%2F.cache%2Fgraft%2Ftestdb%2Fapp-test-pg&sslmode=disable`.
`port` is then not used, and the path of the socket must fit in the 107 bytes
Linux allows (graft refuses a config that does not). Tools in the container
(`docker exec ... psql -h 127.0.0.1`) still reach the server over its
loopback. Moving an existing container to the socket takes `graft testdb
recreate`; `graft testdb down` removes the socket's directory too.

A process running with no_new_privs, as sandboxes run the commands of a coding
agent, cannot start docker where SELinux labels it a container runtime
(`container_runtime_exec_t`): SELinux refuses the move into the runtime's
domain and the desktop shows an alert each time. graft checks
`/proc/self/status` and the label before the first docker command and does
not start docker there. It uses the server as it answers on the DSN instead:
`graft testdb up` and every `test_db` task find it running once it listens
where the config asks, compare the configured settings with what the server
reports, create the source of a migration set over the connection and run
`migrate`. So a database brought up outside the sandbox (`graft testdb up`)
serves the tests run inside it, on a unix socket as well (the sandbox must
allow connecting to sockets; Codex does with network access on). The
sandbox must also let graft write its cache: the lock that keeps worktrees
from changing a container at once is
`<user cache>/graft/testdb/<container>.lock`. A server that does not answer is an error that says to bring it up outside the
sandbox, and so are `down` and `recreate`; the stats get no database CPU.

`tmpfs` mounts a tmpfs of that size in place of the image's data volume
(`/var/lib/postgresql` for postgres 18 and later, `/var/lib/postgresql/data`
before), so no volume is created on disk. It pays off when tests write a lot:
a database per test made with `CREATE DATABASE ... TEMPLATE` copies the whole
template through the WAL each time, and the settings above do not stop those
writes. The price: the size is taken from RAM as the data grows (the cap
must hold the template and every database the tests keep at once), and
everything is lost whenever the container stops or restarts, a reboot
included. The image initializes an empty cluster on the next start, and
since `testdb up` and every `test_db` task run `migrate` each time, not only
after creating the container, the next test run brings the schema back.
`migrate` must therefore work on an empty database.

### A database per test in Go

The package `github.com/aiseeq/graft/v2/testdb` gives every Go test its own
copy of the migrated database, so test binaries run in parallel without
seeing each other's rows:

```go
func TestOrders(t *testing.T) {
	pool := testdb.Connect(t)          // a pinged pgxpool on a copy with the template's data
	testdb.Truncate(t, pool, "orders") // empty tables the template's data would leak into the test
	// testdb.NewDSN(t) gives the copy's DSN for another driver
}
```

`TEST_DB_DSN` names the source, the database `migrate` brings up; its name
must contain `test` and end in `_source`, say `app_test_source`. The package
copies it once into `app_test_template` and makes the copies
`app_test_<test>_<owner>p<random>` from the template; a binary that finds the
version in the source's `schema_migrations` changed rebuilds the template
first. An advisory lock keyed by the source name lets copies be made side by
side and keeps them off a template being rebuilt.

The source is analyzed before the template is made, so the template and
every copy carry the planner statistics of the rows `migrate` loaded:
without them the planner takes the tables for empty and may pick plans
thousands of times slower. On PostgreSQL 15 and later the template and the
copies are made with `STRATEGY FILE_COPY`, which copies the files instead of
writing the whole template to the WAL and costs less CPU while tests write
in parallel. `Truncate` empties the given tables and every table that
references them, as `TRUNCATE ... CASCADE` does, in one statement, and
leaves the empty ones alone: a fresh copy holds only what the template had.

A copy a test is done with goes to the next test of the same binary once its
data is as the template had it, which costs the server a third to a half of
what a new copy does (200 tests on templates of 184 and 525 relations). The
reset sees in the cumulative statistics, flushed as the test's connections
end, which tables the test wrote to, and in their files which it truncated;
those alone are emptied, with the foreign keys off, the template's rows are
copied back, the tables vacuumed and analyzed, and the sequences set back.
The tables the test did not write keep the template's rows. A test that
changed the schema, a planner statistic, a materialized view or any other
system catalog leaves its copy to be dropped, as every copy was before, with
the reason in the test's log (`go test -v`). The reset needs PostgreSQL 15
or later; older servers get a new copy per test. Each pooled copy keeps one
connection open for the reset, whose catalog caches a new connection would
read anew at a cost above the reset's. The copies outlive their binary in
the pool: the next binary's first copy drops the copies of the binaries that
are gone, whose owner key, an advisory lock, nobody holds.

## Tools

```yaml
tools:
  linter:
    go_install: example.com/linter/cmd/linter   # installed @latest; @vX.Y.Z pins the oldest release that works
    tags: [netgo]
    check: [linter, rules]       # its output must contain expect
    expect: some-rule
  migrator:
    go_install: example.com/migrator/cmd/migrator@v2.3.0   # its --version prints "dev"
    tags: [postgres]             # without check/expect: the Go build info is checked
    binary: migrate              # optional: the program on PATH, default the go install name
  shellcheck:
    manual: sudo dnf install ShellCheck   # not a Go module: installed by hand
    check: [shellcheck, --version]
    expect: 'version: 0.11'
```

A binary on PATH says nothing about its version, so a tool counts as present
only when `check` prints `expect`: a version, or a capability such as a rule
the project relies on. Leave the version out of `go_install`, or write
`@latest`, unless the project cannot work with an older release: `expect`
already says what it needs, and a pin nobody raises only grows stale. For an
unpinned tool, graft checks the build against the module's latest release and
installs `@latest`; with a pin it installs that release. Local builds stay in
place, and when the latest version cannot be learned, graft warns and keeps
the build on PATH. `graft tools` reports, `graft tools install` and
`graft init` run `go install` for the failing ones into `tools.bin_dir` from
the user config (by default where go install puts binaries) and check again.
Before installing, graft warns when the new copy will come ahead of another
one on PATH, such as a development build; when PATH still finds another copy
first after the install, graft says where both are.

A tool that go install cannot provide, such as a system package, takes
`manual` instead of `go_install`: it is checked the same way, and when the
check fails graft prints that command and never runs it.

Some Go tools print no real version (`dev`). A `go_install` tool may then
leave out `check` and `expect`: graft finds the program on PATH (`binary`, or
the name go install gives it: the last element of the package path, a `/vN`
suffix skipped) and reads its Go build info. The main package must be the
`go_install` path, the main module version, when `go_install` pins one,
the pinned release or a later one (the pin is the oldest release that works:
a newer copy on PATH is never replaced by an older one) and, with `tags`, the build tags exactly
those. A mismatch is reported as such (`built from
example.com/migrator/cmd/migrator@v2.2.0, pinned v2.3.0`, `built without tags
postgres`), and so is a program with no Go build info. A copy built from a
local checkout carries `(devel)` or a `+dirty` version, a pre-release
`-rc1`, and never matches a pin; without a pin any build of the package
counts. `check` and `expect` go together; a `manual` tool needs both.

## Environments and .env

```yaml
dotenv: .env                    # default; read key by key, never sourced or exported whole
dotenv_template: .env.example   # optional: graft init copies it to dotenv when dotenv is missing
envs:
  local: {}                     # this machine
  test: {ssh: {host: 10.0.0.1, user: deploy}}
  prod: {ssh: {host: 10.0.0.2, port: 2222, key: ~/.ssh/deploy, options: [ConnectTimeout=10]}}
```

Commands bound to an environment are either a **string**, run by that
environment's shell (`sh -c` here, the login shell over ssh), or a **list**,
run as argv. In a list run on this machine, `${KEY}` is replaced by KEY from the
process environment or, failing that, from the `.env` file; a key found in
neither is an error. `$$` is a literal `$`. Commands graft runs on a remote
host never see local `${KEY}` expansion: `$VAR` there belongs to the remote
shell. A list bound to an ssh environment with `${KEY}` in it is a config
error: local values, secrets included, would land in the ssh command line;
write that command as a string, and the server's shell expands it from its
own environment.

With `dotenv_template`, `graft init` creates the dotenv file as a copy of the
template, readable by the owner only (mode 0600), and says so. An existing
dotenv file is never overwritten. A template that does not exist is an error.

The `.env` parser accepts `KEY=value`, `export KEY=value`, `'literal'` and
`"escaped \" \\ \n \$"` values, blank lines and `#` comment lines. In an
unquoted value a `#` after a blank starts a comment: `KEY=abc # note` is `abc`,
as for the shell and the common dotenv parsers. Where those parsers disagree,
graft refuses the line: `KEY=  # note` (empty to the shell and Node dotenv,
`# note` to godotenv) and `KEY=abc#def` (`abc` to Node dotenv only). Write an
empty value as `KEY=` with the comment on a line of its own, and quote a value
that holds `#`. Anything else is an error naming the line too.

## Flags

A project keeps a journal of events that need a human look (failed requests,
stalled jobs, unexpected log errors). `graft flags` shows whether the flag is
raised, that is, whether any open event is not muted by an exception, and closes
events with a reason. Every command takes `--env` (default
`flags.default_env`). Times are shown in UTC, with the zone named; a SQL
`timestamp` column without a time zone is read in the database session's
zone, as PostgreSQL itself reads it.

The flag is meant to be looked at daily, and graft takes that look for whoever
forgot it: once a day per repository and environment, `graft commit` (after
its push) prints the flag of `flags.default_env` and `graft deploy` (after a
deploy that succeeded) the flag of the target's environment, in one line:

```
graft: flag RAISED (prod): 2 events to review, 1 muted by exceptions: graft flags --env prod
```

`graft flags` itself counts as that day's look. The read is bounded by 15
seconds; a journal it cannot read is a warning that fails neither the commit
nor the deploy, and the next try is a day later all the same. An environment
the adapter has no way to (no `psql` or `transport` entry) is skipped.

The exceptions file has one rule per line:

```
# env|class|substring|reason
*|http_failure|timeout calling provider|provider retries on its own
prod,test|log_error|disk almost full|disk alerts come from monitoring
local|job_stalled|*|jobs stall while the laptop sleeps
test|log_error|body:userID=0f1e2d3c|one account's data on test
```

`env` is `*` or a comma-separated list of configured environments; `class` is
the event class; `substring` is matched literally and case-sensitively against
the subject (`*` mutes the whole class); the reason is required and may contain
`|`. A substring `body:text` or `key:text` is looked for in the event's body or
key instead, for a class whose subject is shared and whose object (an account,
an order) is named only there; `subject:text` matches a subject that itself
starts with such a word. The body of an event of a class with a body rule is
read one event at a time when the listing leaves it out. A malformed line is
an error, and `graft check` validates the staged file.
`graft flags mute` appends a rule for the current environment (`--all-envs`
for `*`), using the whole subject unless `--match` narrows it, and then closes
the event; with `--in body` the rule matches `--match` in the body (`--match`
is required), with `--in key` in the key (the whole key by default). Closing
refuses an event that is not open, so another person's note is never
overwritten.

The journal is read through one adapter.

**sql**: a PostgreSQL table read with psql. graft builds the SQL from the
column mapping and sends it on psql's stdin; every answer is one JSON value.
The psql command must be `-X -q -t -A -v ON_ERROR_STOP=1`, reading SQL from
stdin; output that is not JSON is reported as an error.

```yaml
flags:
  default_env: prod
  exceptions: tools/flags-exceptions.conf
  sql:
    table: app_events
    columns:                    # id, class, subject, status, last_seen, note required
      id: id
      class: kind
      subject: subject
      body: body
      status: status
      severity: severity
      key: object_key
      times: times_seen
      first_seen: first_seen_at
      last_seen: last_seen_at
      note: note
      noted_by: noted_by
      noted_at: noted_at
    open_statuses: [open]
    resolved_status: resolved
    actor: agent                # written to noted_by; default graft
    psql:
      local: [psql, '${DB_DSN}', -X, -q, -t, -A, -v, ON_ERROR_STOP=1]
      prod: "sudo -u app sh -c 'exec psql \"$DB_DSN\" -X -q -t -A -v ON_ERROR_STOP=1'"
```

If the table does not exist yet, `graft flags` reports the flag down and says so.

**http**: a JSON API.

```yaml
flags:
  default_env: prod
  exceptions: tools/flags-exceptions.conf
  http:
    list: {path: /api/journal, query: {status: open}, items: data.items, total: data.total, page_size: 500}
    get: {path: '/api/journal/{id}', item: data}
    ack: {method: PUT, path: '/api/journal/{id}/status', body: {status: resolved, note: '{reason}'}}
    fields: {id: id, class: type, subject: subject, body: body, status: status, times: timesSeen, first_seen: firstSeenAt, last_seen: lastSeenAt, note: note}
    open_statuses: [new, in_progress]
    transport:
      local:
        base_url: http://localhost:8090
        headers: {Cookie: 'session={token}'}
        token: [./scripts/dev-token]          # run here; its output fills {token}
        dotenv: [ADMIN_PASSWORD!]             # .env keys the token command gets, as for tasks
      prod:
        # run in the environment (over ssh); {method}, {path}, {body_b64} are shell-quoted
        command: "API_METHOD={method} API_PATH={path} API_BODY_B64={body_b64} sh -s"
        stdin_file: scripts/api-request.sh    # fed to the command
```

Listing follows `limit`/`offset` pages until `total`. Fields are dotted paths
into the response; times are RFC 3339. A direct request that fails shows the
response body; a command transport returns the response on stdout.

## Deploy

graft does not know how to deploy your project; your script does. graft runs
it in the foreground and wraps it with the steps every deploy needs.

```yaml
deploy:
  remote: origin                # HEAD must equal this remote's branch
  branch: main                  # optional: deploy only main; default: the current branch
  untracked: [backend, web]     # optional: where untracked files block; default: anywhere
  load_wait: {budget: 10m, then: start}   # default: how long the deploy's heavy tasks wait for a busy machine in all; then start or fail
  targets:
    test:
      env: test                 # see envs
      deps: [db-smoke]          # tasks run first, as task deps: a quick check here
      run: [bash, deploy/deploy.sh, --env, test]   # argv, run here in the foreground
      version: "cat /opt/app/VERSION"              # prints what the target runs
      deployed_sha: {path: /opt/app/DEPLOYED_SHA, sudo: true}
      release_notes:
        transitions:            # In Progress moves to Testing and goes to the reviewer
          - {from: [In Progress], to: Testing, assign: "<reviewer accountId>", keep_assignee_labels: [internal], require_comment: "How to check"}   # optional: items labelled internal move but stay with their assignee; the others wait for a comment with "How to check"
        fix_version: {prefix: "Product v"}  # add the next unreleased "Product v…" version
        glint: low              # optional: no item moves while glint finds anything of low and up in the deployed commit
      dotenv: [REGISTRY_TOKEN, 'SMTP_HOST?']        # .env keys the script gets, as for tasks
      dotenv_sets: [db]
      status: "cat /opt/app/state.json"
      logs: "docker logs --tail {lines} app-{args} 2>&1"   # graft deploy logs test -- blue
    prod:
      env: prod
      deps: [test]              # the full test task before production
      run: [bash, deploy/deploy.sh, --env, prod]
      requires: test            # prod only gets the version test already runs
      confirm: sudo             # sudo -v before the run: password in a terminal, fingerprint without
      deployed_sha: {path: /opt/app/DEPLOYED_SHA, sudo: true}
      approved_release:         # only commits of checked items go out
        statuses: [Ready for release, Done]  # the item, or a subtask's parent, is in one of these
        prefix: "Product v"     # optional: and in a "Product v…" version every approver approved
        ignore_paths: ["docs/**", "*.md"]   # a commit changing only these ships nothing and passes
        attribute:              # optional: a commit whose message names no item, or the wrong one, as a commit of the item
          - {commit: 0123456789abcdef0123456789abcdef01234567, item: PROJ-123, reason: "the key was left out of the message"}
      release_notes:
        transitions: [{from: [Ready for release], to: Done}]
        subtasks: true
        comment: "Released {release}, version {version}"   # this target's text; {release} needs release_version

release_notes:
  jira:
    project_keys: [PROJ]        # required: SHA-256 is shaped like a key too
    comment: "Deployed to {target}, version {version}, commit {short}"   # default
    skip_statuses: [On Hold]    # leave these items alone: no note, no move
    skip_keys: [PROJ-100]       # these too, and no subtask follows them; a subtask named by its own key goes as any item
```

`graft deploy [--redeploy] [--unapproved] <target> [-- args]`:

0. Check that every required key of `dotenv` and `dotenv_sets` and every
   `${KEY}` in `run` is set, and everything the `deps` tasks need, listing all
   that are not; nothing else happens before this passes.
1. The work tree must be clean; `git fetch <remote>`; HEAD must equal the remote
   branch. Then every other remote is fetched (a failed fetch stops the
   deploy), and the deploy is refused while one's branch of the same name has
   commits whose change the deployed branch lacks, a pull request merged on a
   mirror say: the refusal names the remote, the count and the first commits,
   with the merge and push that bring them in. Content counts, not ancestry:
   the exact changed lines, including whitespace, must already be present for a
   rebased or cherry-picked commit to count as there. Merge commits are not
   counted, the non-merge commits they bring are.
2. With `requires`, the required target's `version` must print the version
   being deployed: a quick read-only check, so it fails before a long deps run.
3. With `deployed_sha`, read the commit the target runs. Then, before any
   refusal, two lines show what changes there: what the target runs, its
   version (from `version`, or else the version files of that commit), the
   commit and the remote branches at it (or else containing it), as
   `graft deploy versions` shows it; then the commits the deploy brings and
   those it takes back (`git log <deploying>..<deployed>`), the hotfix a
   deploy from another branch would drop. `graft deploy check` prints them too.
   When the commit the target runs is the one being deployed, the deploy is refused (`test already runs <sha> <subject>;
   pass --redeploy to deploy it again`) before deps, sudo and the script;
   `--redeploy` deploys it again. With `deployed_sha.sudo`, this read runs
   `sudo` in the target environment before the confirmation prompt: on this
   machine it may ask for its own password there. A target without
   `deployed_sha` is deployed whatever it runs.
   With `approved_release` (it needs `deployed_sha` and `release_notes.jira`),
   every commit going out, merges aside, must name an item that passes:
   with `statuses`, the item is in one of them, or it is a subtask whose
   parent is; with `prefix`, the item has a fix version that begins with
   `prefix` and that has approvers, every one of whom approved it in Jira,
   or it is a subtask whose parent has such a version; with both, both. One
   passing item lets the commit out. A commit let out by
   a done-category item gets a line of its own: it may only refer to old
   work. A revert (`Revert "…"`, `revert:`) passes, but a revert of a revert
   (`Revert "Revert "…""`, `Reapply "…"`) brings the work back and is checked
   like any commit. A commit that changes only `ignore_paths` passes too
   (globs as in exceptions; the version file and `sync` files that graft
   commit bumps do not count). Otherwise the deploy stops here, before deps
   and the script, listing each commit outside the release: one naming no
   item, or naming items that do not pass, and why. With no deployed commit
   recorded, or Jira unreachable, it stops too. After the commits it lists
   their items once each, with summary, status and the fix versions of
   `prefix` and their approval: what to fix in Jira. `--unapproved` skips the
   check with a warning. `graft deploy check <target>` runs this check and
   the others before the deps (unpublished work, the window, `requires`, the
   hold) without deploying: it lists every refusal rather than the first,
   records nothing (not even the hold's candidate) and reads the deployed
   commits with `sudo -n`, so a server that wants a sudo password answers
   with an error instead of a prompt.
   `attribute` ties a commit already in the history whose message names no
   item, or the wrong one (a standing parent instead of its subtask, say), to
   the item it belongs to, without rewriting history: the commit is checked
   exactly as a commit naming that item alone (`statuses`, a subtask by its
   parent, `prefix` with every approval), the deploy prints `attributed to
   PROJ-123: <reason>` for it (`attributed to PROJ-123 instead of PROJ-100:
   <reason>` when its message names others), and the release notes, the release page,
   `graft deploy cutoff`, `.withheld`, the window versions and, on the
   hotfix `base` target, the hotfix label check count it as the item's, and
   so its revert too; a commit that brings the reverted work back (`Reapply`,
   a revert of the revert) needs an attribute of its own.
   `commit` is the full hash (40 lowercase hex digits, 64 in a SHA-256
   repository), `item` a key of `project_keys`, `reason` is required. A short
   or repeated hash, a missing reason, a commit the repository does not have
   and a commit whose message names the attributed item alone already are config errors: the
   first ones when the config loads, the last two in `graft check` (and so
   `graft commit`) and every `graft deploy` command. Each target has its own
   list.
   Before it, an approved release must contain the commit the target runs:
   a hotfix or a release branch not merged back would be taken back out. The
   refusal names the branches that have it and how to merge each (see
   Release windows). `--unapproved` does not skip this.
   With a `window`, a hotfix branch and a held candidate, more checks come
   here; see Release windows below.
   With `integrations`, the target must configure every integration the
   target from `requires` runs; see Integrations below.
4. Run the `deps` tasks, exactly as a task's deps run: in order, each at most
   once (deps of deps included), a `test_db: true` task with the test database
   up and its DSN. A failure ends the deploy before the deploy script runs or
   anything is written in the target environment. A `deps` entry that is not
   a task is a config error.
5. With `confirm: sudo`, run `sudo -v`. In a terminal it asks until the password
   is given (Ctrl-C stops). Without a terminal it passes only on cached sudo
   credentials or a PAM method that types nothing (a fingerprint reader): while
   the reader refuses (no finger in time, an unknown finger) graft prints the
   attempt and asks again, for at most 10 minutes (Ctrl-C stops). Any other
   refusal ends the deploy, since nobody can type a password there. The reader
   is recognized by the messages of pam_fprintd, so sudo runs with `LC_ALL=C`.
   On a host with `pam_faillock`, every refused attempt counts as a failed
   login.
6. Run `run` plus `args` in the foreground, with `GRAFT_DEPLOY_TARGET`,
   `GRAFT_DEPLOY_ENV`, `GRAFT_DEPLOY_SHA`, `GRAFT_DEPLOY_VERSION` and
   `GRAFT_DEPLOY_PREVIOUS_SHA` in its environment, plus the listed `.env`
   keys (an optional `KEY?` set nowhere is left out). Between building and
   shipping, the script can call `graft deploy check-head`.
7. Check again that the tree is clean and HEAD has not moved.
8. Record the deployed commit and post release notes: a comment on every
   `project_keys` item mentioned in the delivered commits (subjects and bodies)
   or tied to one by the target's `approved_release.attribute`, but not one
   named only by reverts and the commits they revert among them: those take
   the item's work out (a release branch's reverts reaching the target that
   holds the candidate, say), and graft says so,
   and with `transitions`, a move of the items in one of a transition's `from`
   statuses to its `to` (a status is in one `from` at most); an item
   elsewhere, paused or not started, gets the comment and keeps its status.
   A transition with `assign` (an account id) hands the moved item over to
   that account, the reviewer of Testing say, unless it is assigned there
   already or has one of the transition's `keep_assignee_labels`, or its
   parent has one: such an item, technical work nobody checks on a screen
   say, moves and stays with its assignee. With `require_comment` (text,
   "How to check" say) an item to be handed over moves only when a comment
   of the owner of the Jira token written since the item last came into
   the transition's `from` from elsewhere (a return from review starts
   anew) contains that text in any case; a subtask following its parent
   counts the parent's comment too. Without one the item keeps its status
   and assignee, the deploy prints `graft: KEY: NOT MOVED to Testing: …`
   and goes on with its exit status unchanged, and the item waits: the next
   deploy to the target, or `graft deploy handover <target>` once the
   comment is written, moves it, and an item that has left `from` by hand
   waits no more. `graft deploy handover` fails while any item still waits.
   With `glint` (a severity: `low`, `medium`, `high`, `critical`) no item
   moves by the transitions while `glint check . -s <severity> -o json`
   reports anything in the deployed commit: an item is done only with the
   project clean, the commit gate failing on the high findings alone. graft
   runs glint once a deploy, in the work tree when it is at the deployed
   commit and clean, else in a work tree of its own at that commit, and
   counts the report's findings whatever glint's exit status; a run without
   a report, or with rules that panicked, holds the items too. A held item
   keeps its status and assignee, the deploy prints `graft: KEY: NOT MOVED
   to Testing: glint: N findings of low and above in <commit>
   (release_notes.glint); …` and goes on, and the item waits as for its
   comment: the next deploy with the findings fixed, or `graft deploy
   handover <target>`, which checks the commit the target runs again,
   moves it. At a release acceptance, the findings left for an item of
   their own, `--glint-ticket KEY` on the deploy or the handover names that
   item, an open one of `project_keys`, read before the deploy runs its script: one
   not open refuses the deploy. The items move, each saying `KEY moved to
   Testing with N glint findings in <commit>: PROJ-123 takes them
   (--glint-ticket)`; graft cannot tell an acceptance from another deploy,
   so naming the item is the acceptance's to do. The sweep (below) moves no
   item to its done status either while glint finds anything in the commit
   the target runs: it lists the item instead.
   The release note itself must not contain the text. A key mentioned in a
   commit does not make the item yours, so only open items of the owner of
   the Jira token get notes: not in a done-category status (Done, Won't do),
   a to-do-category one (To Do, Backlog: work not started) or `skip_statuses`, and assigned to the owner now or at some point in
   the item's history (an item handed over for review keeps its notes). Other
   items are left alone with a warning, and so is an item whose commits in
   the deploy are all of the Conventional Commits types docs, test, style or
   ci: they change nothing the target runs, so the item is not delivered
   (no note, fix version or move). A merge commit delivers what the commits
   it brings do: an item the deploy's other commits name goes by them, and
   one only the merge's message names is delivered when the merge brings a
   commit of another type. A failed comment or move is printed
   as a warning and does not change the exit status.
   With `subtasks: true` (it needs `transitions`), subtasks follow their
   parent: when a delivered item is in, or has just reached, the `to` of a
   transition, its subtasks in that transition's `from` move there by it,
   `assign` included. A subtask further on, Ready for release when its
   parent comes to Testing, say, stays, and so do subtasks that are not
   yours, as above. The single `transition` and `from` of graft 1.x are replaced by
   `transitions`; the old keys are a config error that shows the rewrite.
   With `release_page`, the release is published in Confluence; see Release
   pages below.
9. With `fix_version`, every item that gets a note also gets a fix version:
   the version of its Jira project that is neither released nor archived,
   whose name begins with `prefix`, and whose release date is the nearest,
   that is the earliest (an overdue version still unreleased is the next
   one); versions without a date come after the dated ones. A version that
   anybody has approved in Jira is passed over: its approved set of items is
   final, and they go to the next one. graft never guesses:
   no such version, two sharing the nearest date, or several undated ones
   and no dated one are errors naming the candidates. With `name: "Product
   {dd}.{mm}.{yy}"` instead of `prefix`, the versions are those whose names
   follow the template exactly, the earliest day in the name first; a name
   that looks like the template but does not follow it ("Product.19.10.26")
   is an error naming it. The version is added
   with an `update` `add`, so the item's other fix versions stay; an item
   that has it already is left as it is, without an edit request. Editing
   the item is the only permission `fix_version` takes; `release_version`
   (see Release windows) creates and releases versions. The release is live by then, so a fix version that is
   not set (no version to pick, Jira unreachable, the edit refused) does not
   stop the rest: graft prints `graft: PROJ-123: fix version NOT SET: <why>`,
   one line per item, finishes the notes, and exits 1 with the list of items
   left without it. An item moved but not handed over to `assign` is
   reported the same way: `graft: PROJ-123: assignee NOT SET: <why>`.

graft exits with the deploy script's exit status, or 1 when the script
succeeded but a fix version or an assignee is not set. Two deploys to the same
target from one repository wait for each other.

`graft deploy status <target> [-- args]` and `graft deploy logs <target>
[--lines N] [--grep RE] [-- args]` run the target's `status` and `logs` in
its environment. `{args}` receives the arguments as in tasks: verbatim,
shell-quoted by graft, all of them as a whole word, exactly one inside a word;
do not put it inside quotes of your own. `{lines}` is filled in `logs`. A
command without `{args}` refuses arguments. With `deployed_sha`, status first
names the deployed commit with its subject from the local repository, reading
the file with `sudo -n` so that it never waits on a password prompt.

Jira credentials are the user's, not the project's: `jira.env_file` in the
user config (see Install).

### Sweep

The release notes of a deploy see only its own commits: the first deploy
of a target, an item whose last commit went out earlier, a parent whose
commits all name its subtasks get no move. `release_notes.sweep` on a
target reconciles the items with the commits the target runs, after each
deploy to it, once a day after a `graft commit`, and on `graft deploy sweep
<target> [--apply]`:

```yaml
    prod:
      release_notes:
        transitions: [{from: [Ready for release], to: Done}]
        sweep: {apply: true}   # days: 30, stale_days: 3, done: Done by default
```

The sweep takes the commits of the last `days` on any remote branch or on
the target, merges left out, and groups them by the item each is the work
of: the first key of its subject, or else a line of its body that is a key
alone, as graft commit adds the key of the branch, or its
`approved_release.attribute`. A key the body only mentions ("after
PROJ-12") refers to another item and counts for nothing; so does a key
that opens the subject only as a mention ("answers on PROJ-12"): graft
cannot tell it from work, give such a commit the key of its own item first.
A revert and the commits it reverts deliver nothing; a commit withheld from
the target is not on it, though its history has it.

A commit of the trunk the target does not run by history is on it by
content when, put onto the commit the target runs, it changes no file but
the version files and those `approved_release.ignore_paths` names: a
cherry-pick to a release branch, two commits taken in one, a commit of docs
alone. An item whose commits change nothing but `ignore_paths` moves saying
its work `ships nothing to <target>`: it needs no deploy. A commit that also
changes other files, taken to the target without its ignored ones, is on
it. A commit the target runs names what it took with the line `git
cherry-pick -x` adds, `(cherry picked from commit <sha>)`, one per commit:
the commits named are on the target and the pick is in the trunk once they
are, whatever shape the release needed; without the line a pick of a
change the trunk has changed further since is not recognised and stays
listed. A commit the target runs is in the trunk by content the same way.

Of the token owner's items (assigned
to them now or once), not in `skip_keys` (a subtask of an item there is
judged by its own key):

- an item in the `from` of the target's transition to `done`, all of whose
  commits the target runs, moves to `done`; with `approved_release`, only
  when it passes as a commit of it would (`statuses`, a version every
  approver approved), otherwise it is listed with the reason;
- a parent all of whose subtasks are done, whose work and its subtasks'
  the target runs in full, moves to `done`; with `approved_release.prefix`,
  only in an approved version. A parent not started, or in a status no
  transition or `approved_release` names (Paused), is listed instead: it
  was put aside by hand;
- listed by hand, nothing moved: a done item with a commit outside the
  trunk or not on the target yet; an item not started (To Do), or in
  `skip_statuses` (put aside by hand: Blocked, On Hold), with commits in
  the trunk; an item in any other status (In Progress, Testing, Paused
  unless `skip_statuses` names it), all its work on the target, whose
  newest commit is `stale_days` old.

Without `apply` the sweep only prints the moves it would make: start that
way and switch it on once the list reads right. `graft deploy sweep` moves
only with `--apply`, whatever `apply` says, and reads the commit the target
runs with `sudo -n`, the record below standing in when that needs a
password; the daily sweep reads no server
and takes the commit the last deploy or `graft deploy sweep` from this
repository recorded (in the user cache), so it waits for one of them. A
failed sweep is a warning: the deploy and the commit go on.

### Release windows and hotfixes

```yaml
version:
  mode: file
  build_branches: ['hotfix/*', 'release/*']   # their builds are 1.2.3.1, 1.2.3.2
deploy:
  hotfix:
    branches: ['hotfix/*']      # these go out any time, from what base runs
    label: hotfix               # every item a hotfix commit names carries it
    base: prod                  # the target a hotfix branch starts from
  targets:
    prod:
      requires: test            # test holds the candidate
      window:
        days: [mon, thu]        # mon tue wed thu fri sat sun
        at: "15:00"
        until: "19:00"          # required: when the window closes
        tz: Europe/Berlin
        cutoff: {days_before: 1, at: "18:00"}   # optional: when test starts holding the candidate
      approved_release: {statuses: [Ready for release, Done]}
      merge_into_trunk: true    # optional: record the release branch on the trunk after the release
```

- Outside its window a target refuses, before `requires`, deps and sudo,
  naming the next window. A hotfix branch goes any time.
- From the cutoff until the window closes, or until the windowed target
  runs the candidate or work on top of it, the target named in `requires`
  holds the release candidate: the commit it ran at the cutoff, recorded
  beside its `deployed_sha` (`.candidate`) on the first deploy there after
  the cutoff, or when the hold is over. Every deploy there after the release
  in the window passes the hold, not only the first.
  Meanwhile it takes only commits on top of the candidate that revert, that
  change only the windowed target's `ignore_paths`, or that the windowed
  target already runs (a merged hotfix); a hotfix branch goes there too.
- `graft deploy cutoff <target>`, run on the trunk, prints the window, the
  candidate and the commits of the candidate that `approved_release` would
  refuse, grouped by item, with the commands that take them out: a branch
  `release/DD.MM.YY` from the candidate, a reverse patch per commit without
  the version files (`git diff <sha>^ <sha> -- . ':(exclude)VERSION' | git
  apply -R --3way --index`) and one `graft commit -m 'revert: …' -m 'This
  reverts commit <sha>.'` per item; then the deploys, and after the release
  `git merge -s ours` of the release branch into the trunk. With nothing to
  revert it prints the branch and the deploy alone.
- A deploy from a `build_branches` branch withholds the commits a revert in
  it names: they pass now, the trunk keeps them, and graft records them
  beside `deployed_sha` (`.withheld`). Every later release checks them with
  its own commits until they pass or a revert takes them out of the trunk.
  In any deploy, a previously withheld commit that remains reverted in the
  deployed history and is absent from its tree stays in `.withheld` and is not
  checked again. Once its change is present in the deployed tree, it is checked
  again.
  Release notes leave out withheld commits and the reverts that withheld
  them; a withheld commit that a later release lets out gets its notes
  there, though it is outside that release's range.
- A hotfix branch must start from the commit `base` runs and every commit on
  it must name an item with `label` (a revert, or a change only to the base
  target's `ignore_paths`, passes). The window, the hold and
  `approved_release` do not apply; `requires` does. After the hotfix reaches
  `base`, graft prints how to merge it into the trunk; the next release from
  the trunk refuses until it has.
- `merge_into_trunk: true` on a target makes the deploy record a branch
  other than the trunk on the trunk once the target runs it, so that the
  next deploy from the trunk finds what the target runs among its
  ancestors: a merge commit keeping the trunk's tree, as `git merge -s ours`
  makes, pushed to the trunk's remote (`deploy.remote`, whose HEAD names the
  trunk) and then to the others, the local trunk following when it was where
  the remote was. It merges only when the trunk has all the branch's work
  by content: every commit of the branch the trunk lacks is a revert (work
  the release withheld, which the trunk keeps), picks onto the trunk
  without changing it, the version files aside, or is a port of trunk work:
  every item it names (by `release_notes.jira.project_keys`, else
  `ticket.pattern`) is named by a trunk commit since the branch left it,
  the trunk having moved on and reworked the same lines. The merge lists the
  commits that passed by their items alone. A fix made on the branch and not
  in the trunk would be dropped by such a merge: the deploy, live by then,
  names those commits, prints the commands that merge after the trunk has
  them, and exits 1. It needs `deployed_sha`.
- `release_version: {enabled: true, name: "Product {dd}.{mm}.{yy}", hotfix:
  "Product {dd}.{mm}.{yy} hotfix"}` on the windowed target keeps one Jira
  version per window (`enabled` is required; false leaves the Jira versions
  alone and needs no permission, while `name` and `hotfix` still name the
  release for `release_page` and `{release}` in the target's
  `release_notes.comment`). Enabled, it needs the Manage versions permission
  (MANAGE_VERSIONS; in a team-managed project the Administrator role):
  without it every step below is an error saying so.
  - A deploy to the held target that reverts commits of the candidate moves
    their items from the window's version to the next window's (created when
    missing) and posts them no note: the version QA approves lists what goes
    out.
  - A release refuses, before deps, while the window's version lists an item
    none of its passing commits names.
  - After a release the window's version gets the build (`build <version>,
    commit <short>`) appended to its description and is released on the
    window's day; the earliest unreleased window version is renamed to that
    day when it carries another (the window moved), and created when there is
    none. Then the next window's version is created with its release date.
  - A hotfix reaching its base puts its items into the day's `hotfix`
    version (created when missing) and releases it with the build.
  Failures after the release is live print `graft: versions NOT KEPT: <why>`
  and make the deploy exit 1.
- `GRAFT_NOW` (RFC 3339), when set, is the time these checks use instead of
  the clock: for tests and for rehearsing a cutoff.

### Freeze

Add `freeze` to a release target that has `requires`; it holds that required
acceptance target while the customer reviews a Jira version. Freeze requires
`approved_release.prefix`, `deployed_sha` on both targets, and
`release_notes.jira.project_keys`. `freeze.types` allows items of those Jira
types that joined the version after the freeze. `freeze.advisory` names tasks
that must be direct deps of the frozen target, its required target, or a
top-level gate step. They run in configured order and failures warn and are
recorded during deploys to either target and the `graft commit` gate on the
frozen branch. Without this setting, deploy behavior is unchanged.

For example, with `release_notes.jira.project_keys: [PROJ]`, configure both
targets with `deployed_sha`, set `approved_release.prefix: "Product "` on the
frozen target, and list `lint` among that target's or its required target's
deps (or as a top-level gate step):

```yaml
freeze: {branch: release/{version}, types: [Bug], advisory: [lint]}
```

Run `graft deploy freeze <target> <Jira version>` from the trunk to cut and
push `release/{version}` and the annotated `freeze/<target>/<version>` tag.
The version prefix is removed from `{version}`. While the tag is on a remote,
the required target accepts only that branch and commits naming an item in the
frozen version. Items added to the version after the freeze pass only when
their type is in `freeze.types`. Use `graft deploy --past-freeze 'reason'
<required target>` to pass any of these checks; graft warns and records the
bypass. A hotfix still follows the configured hotfix checks and is recorded
in the freeze journal. Advisory tasks run on the frozen branch during deploy
and at the `graft commit` gate; failures warn and are recorded without stopping
the operation. A successful deploy of the frozen branch to the release target
removes the freeze and journal tags from every remote only after `deployed_sha`
and the release page journal succeed. If no release page is configured, the
journal is reported as having no page and `deployed_sha` is sufficient. `graft deploy thaw
<target> --reason 'reason'` ends it manually and removes those tags too.

Freeze and journal tags live on the remotes. To keep removed tags from
returning in local clones, enable `fetch.pruneTags` or set
`remote.<name>.tagOpt --no-tags` for each remote.

### Release pages

```yaml
deploy:
  targets:
    prod:
      release_page:
        parent: "123456"                 # the id of the page release pages go under
        title: "{release}: release notes" # the default; {release} is required
        fix_types: [Bug]                  # the default: these are fixes, the rest changes
        labels: {changes: Changes, fixes: Fixes, moved: Moved to the next release}
```

- After a release graft publishes a Confluence page of what went out, with
  the Jira credentials (a Confluence of the same site). `{release}` is the
  window's version (`release_version.name`), the hotfix's
  (`release_version.hotfix`), or the approved versions the release took its
  items from (`approved_release.prefix`); one of them is required.
- A new page starts with a table of the release (linked to its Jira
  version), the day and the target. Then a section per build: a heading
  (`labels.build`, default `Build {version}, commit {short}`) and tables of
  the items the release took out (the ones `approved_release` passed, or a
  hotfix's labelled items) by issue type, with links, summaries and types,
  and the items of withheld commits, which wait for the next release.
- A title is unique in a space: a second release with the same name, such as
  a second deploy in one window, adds its build section to the page; a
  build the page lists already is left alone.
- Labels: `release`, `date`, `target`, `build`, `changes`, `fixes`, `moved`,
  `none`, `issue`, `summary`, `type`.
- The release is live by then: a failure prints `graft: release page NOT
  PUBLISHED: <why>` and makes the deploy exit 1.

### Integrations

```yaml
deploy:
  targets:
    prod:
      requires: test
      integrations:
        command: app --print-integrations     # one integration a line
        exceptions: deploy/integration-exceptions.conf
```

- `command` runs in the environment of the target and of the one from
  `requires`, and prints the integrations each configuration turns on, one a
  line. An integration `requires` runs that this target lacks stops the
  deploy before deps and sudo, unless `exceptions` excuses it.
- `exceptions` is a file of `integration|reason` lines, `#` comments and
  blank lines skipped. A line without its reason, a name twice or a missing
  file is an error; an exception the comparison does not need any more (the
  target has it now, or `requires` no longer runs it) is a warning.
- A command that only the new build answers right, the build asking its own
  configuration by the rules it starts with, takes `{staged}`: `command: sudo
  -u app {staged} --print-integrations` with `running: /opt/app/bin/app`,
  the build the target from `requires` runs, as `{staged}` there. graft
  deploy then leaves the comparison to the deploy script, which calls
  `graft deploy check-integrations <uploaded build>` once the build is on the
  target and before it switches to it; graft records the deploy only after
  that call passed for the commit being deployed.

### Version modes

- **file**: the version lives in `version.file`. `graft commit` bumps it
  (patch by default) together with every `sync` file; each must exist and
  contain the version, or the bump fails before anything is written. The
  checks verify in the index that the sync files agree with the version file,
  which catches a hand-resolved merge conflict. `graft release` tags the
  committed version.
- **git-tag**: no version file; versions are annotated tags `v<X.Y.Z>`.
  `graft version` prints `git describe` from the highest version tag reachable
  from HEAD, or `no version tags yet (HEAD <hash>)`. `graft release` tags the
  next patch (or `--minor`, `--major`, `--version X.Y.Z`); with no version tag in
  the history the first release is **v0.1.0**. A HEAD that already carries a
  version tag is refused, unless `--version` names the new one: a release run
  after a commit that failed would tag the same code again. Tags that are not
  plain `vX.Y.Z` (such as `v1.0.0-rc1`) are ignored.
- **none**: no version at all.

`graft release` only tags a HEAD that every push remote already has.

### Checks

- **Secrets**: the staged content of added and modified files is scanned line
  by line for PEM private keys, AWS access key ids, `PGPASSWORD=` assignments,
  quoted `sk_live_`, `re_`, `GOCSPX-` keys and quoted JWTs, plus
  `extra_patterns`. Findings name the file, line and kind, never the secret.
  Binary files are skipped. Files already committed are not rescanned.
- **Large files**: a binary file (NUL byte in the first 8000 bytes, git's own
  test) is refused unless its extension is in `binary_extensions`; binary
  files over `max_binary_bytes` and text files over `max_text_bytes` are
  refused, and so is a symlink to an absolute path (`/x`, `C:\x`), which
  leads into this machine and nowhere in another clone: one made by mistake
  inside the tree is staged by a commit that stages all.
- **Reverts**: a commit must not silently take back merged work, a tree from
  an old base committed on a fresh parent say. For each staged file and each
  of the `depth` recent commits of HEAD's first-parent line that changed it:
  when that commit's diff of the file applies to the staged tree and did not
  apply to HEAD's, the file takes it back, a deleted file as much as changed
  lines. A revert that is meant names the commit (7 characters of its hash
  or more, the short form git prints) or the file in the message; `git revert` names the commit, and in
  a rebase, merge, cherry-pick or revert the message git prepared counts.
  Moves, the version files and `exceptions` do not count. A partial
  take-back, part of a commit's diff of a file, is not seen.
- **Leaks**: with `leaks` in the user config, a commit to a repository that
  has a remote under `public_remotes` is checked for private names in the
  lines it adds and in its message; a line the commit removes elsewhere,
  in the same or another file, is moved, not added. A remote under
  `private_remotes` is private even under a public prefix. The lists live in the user config, never
  in the public repository:
  - **names**: camel-case functions, methods and types of at least
    `min_name_length` characters declared in `private_sources` (parts of test
    names included, the test's own name not). A name the public Go code in
    GOROOT and the module cache also uses is common and dropped; modules under
    `public_remotes` or `private_remotes` and the private sources' own
    modules do not count as public code. The first run indexes the module cache, later runs only the
    new modules (`<user cache dir>/graft/leaks`). A name the repository
    declares in its own non-test Go code is its vocabulary; a mention in
    tests, fixtures, comments, docs or the message is a finding.
  - **terms**: every match of a `terms_file` regexp.

  A finding names the file, line and the name or term. A line containing
  `graft:leak-ok <reason>` passes; `leaks.allow` passes a name or term
  everywhere. `graft leaks <revision>…` runs the same scan over existing
  commits, each against its parent, to audit a history. Pass any `git rev-list`
  revisions as separate arguments; for example, `graft leaks --all` scans all
  branches and tags. A clone has no pull request refs (`refs/pull/*`) unless
  they are fetched: `git fetch origin '+refs/pull/*:refs/pull/*'` first.

## Worktrees

`graft worktree add [--trunk <ref>] <branch> [<base>]` makes a worktree at
`<user cache>/worktrees/<repository>/<branch, / as ->` (`~/.cache` on Linux),
out of the directory the repositories live in, on the branch: a new one from
`<base>`, by default the trunk (below), or the existing one. Use `--trunk` to
select the starting ref when no `<base>` is given; `--trunk` and `<base>` cannot
be combined. A relative path in
go.work's `use` and `replace` or go.mod's `replace` that leads out of the
repository (`../kit`) is linked at the same place beside the worktree, so the
worktree builds as the repository does; a link already there must lead to the
same directory, or nothing is made.

A checkout lacks what git does not track: the local setup (`.env`) and the
files the project generates. `worktree` in `.graft.yaml`, as the branch has
it, brings them in, so the gate and the tasks run in the new worktree as in
the main one:

```yaml
worktree:
  copy: [.env]               # files of the main worktree, copied with their permissions
  setup: [generate-config]   # tasks run in the new worktree after the copy
  link:
    - key: [frontend/package-lock.json] # matching files identify shared dependencies
      paths: [frontend/node_modules, frontend/admin-app/node_modules]
      setup: frontend-deps    # run when a key differs or a directory is missing
```

For each `link` group, `add` links its untracked directories to the main
worktree when all key files match and all directories exist there. Otherwise,
it runs the group's setup task in the new worktree, once for all the groups
that name it, before the tasks of `setup`; a group whose key matches but
whose setup runs for another group is not linked either (`not linking
<paths>: graft <setup> installs it with another group`). A group needs
`key`, `paths` and `setup`; a path the checkout tracks is an error. A
directory link the system does not make (Windows without the right to make
symlinks) is warned and the group's setup installs instead. A setup task run
in a worktree, on its own or as a dependency or step of another task, first
removes its groups' links to the main worktree (links there only, never a
directory), so it installs into the worktree, not into the main one. `graft
gate` and the gate of `graft commit` in a worktree warn, one line a path, of
a link whose group's key file now differs from the main worktree's:
`worktree.link <path>: a link to the main worktree, but <key> differs from it:
run graft <setup>`; the gate goes on.

A file of `copy` missing in the main worktree, or one the checkout has
(tracked), is an error, and so is a failing `setup` task; the worktree stays,
for the fix and a rerun of the task there.

`graft worktree clean` lists every worktree but the main one and every local
branch no worktree holds; with `--apply` it removes the ones whose work is in
the trunk:

- the trunk is `--trunk`, or the HEAD branch (`refs/remotes/<remote>/HEAD`) of
  `deploy.remote`, or of the only remote; every remote is fetched first;
  without a remote it is the local `main` or `master` when only one of them
  is there, else the branch of the main worktree, and `--trunk` is needed
  only when both are there and the main worktree is on neither (the same
  trunk serves `add`, `sync` and `deploy sweep`);
- in work, left alone: locked (`git worktree lock --reason`, the reason
  shown), a process works in it (its working directory or a file it has open
  there, read from /proc, so Linux only), used within `--min-age` (its HEAD
  reflog, and the changed files' times when there are uncommitted changes),
  or uncommitted changes the trunk lacks, those files named. A tool that
  runs in a worktree in turns, with pauses where no process is there, locks
  it for the whole job and
  unlocks it when the job is done: between the turns nothing else shows the
  worktree is taken, and `git worktree remove` refuses a locked one too;
- in the trunk: merging it into the trunk (`git merge-tree --write-tree`)
  gives the trunk's tree, the version files graft bumps aside. Ancestry and
  patch ids are no measure: a rewritten commit has another hash, and a commit
  with the same subject may carry other content. Uncommitted changes and
  untracked files (ignored ones aside) count as a commit on HEAD: when the
  trunk has them by content, merged there by other commits, the worktree
  goes as a clean one, after a second look that nothing changed since the
  check;
- a worktree is removed with `git worktree remove` (`--force` for one whose
  uncommitted changes are in the trunk), then its branch, and the
  branch of the same name on every remote whose copy is in the trunk too,
  only while it is still at the commit checked (`--force-with-lease`):
  commits pushed there after the fetch keep it, listed as `kept`; one
  deleted there after the fetch is listed as `already gone`, no error;
- the rest is listed: `unmerged` with the lines or conflicts it adds, and
  `check` when its commit subjects are all in the trunk but its content is
  not: it looks merged, a look decides.
- with `--apply`, the golangci-lint caches (see Tasks) of work trees that
  are gone, in any repository, are removed too.

`graft worktree sync [--trunk <ref>]`, run in a worktree, moves its work onto
the fresh trunk: it fetches every remote, rebases the branch's own commits
onto the trunk (found as for clean) and brings the uncommitted changes back
uncommitted, untracked files left where they are (`git rebase --autostash`).
Nothing is committed or pushed, so a worktree is synced in the middle of its
work without a temporary commit past the hooks. Uncommitted changes that
conflict with the trunk are left with their markers and kept in the stash
too (resolve, then `git stash drop`); a commit of the branch that conflicts
stops the rebase (`git rebase --continue` or `--abort`); an untracked file
the trunk adds stops it before anything changes.

## Go build cache

The go command keeps a build cache entry for five days after its last use and
gives no way to shorten that; on a machine that builds many projects and
worktrees the cache holds days of builds nobody will repeat. `graft gocache
trim` removes the entries of `go env GOCACHE` unused for longer than
`--older` (25h by default: a day, plus the hour go waits before it marks an
entry used again), the way go's own trim does: files `<hash>-a` and
`<hash>-d`, and directories `<hash>-d` with the executables `go run` and `go
tool` built, in the subdirectories `00`…`ff`. The fuzz corpus and the cache's
own files stay, and a directory without the README go writes there is
refused. Without `--apply` it only reports what it would remove and what it
keeps. A removed entry is a cache miss: go rebuilds the package the next time
it is needed, so a project not built for a day builds cold once, and a
project in use stays warm. Run it daily from a timer, for instance a
systemd user unit with `ExecStart=` the absolute path of graft and `gocache
trim --apply`, and `OnCalendar=daily`: systemd does not look in `~/bin`.

## Development

graft builds, tests and commits itself through its own `.graft.yaml`:

```sh
go run . smoke     # the gate: gofmt check, vet (host and windows), glint, the tests but the docker container ones
go run . all       # the gate (go vet, go fix, smoke) + race, the docker tests included
go run . install   # ~/bin/graft, after the pilot's gate (see Install)
go run . commit -m "..."
```

Tests of the SQL adapter need PostgreSQL and psql, the test database tests
need docker: the `test` task brings up the test database and passes its DSN
in `GRAFT_TEST_PG_DSN`. Plain `go test` skips those tests and says so.

## License

MIT
