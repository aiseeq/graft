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
go install github.com/aiseeq/graft@latest
```

or, from a checkout, `go run . install` (atomically replaces `~/bin/graft`).
Requires git 2.31 or newer. A project can require a graft version with
`graft: ">=X.Y.Z"` in `.graft.yaml`: an older graft refuses to work on it and
the hooks' install hint names that version.

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
  private_sources: [~/work/projectA]          # private projects: their Go declarations are private names
  terms_file: ~/.config/graft/private-terms   # "kind: regexp" per line: keys, domains, addresses, names
  min_name_length: 10                         # default 10
  allow:
    - {term: SomeName, reason: why this name is fine everywhere}
```

## Commands

| Command | What it does |
|---|---|
| `graft commit -m "fix: ..."` | lock, gate, bump patch (version mode `file`), `git add -A`, checks, commit, push to every remote |
| `graft commit --minor` / `--major` | same with a minor / major bump (version mode `file` only) |
| `graft commit -F msg.txt` / `-F -` | message from a file / from stdin, byte for byte |
| `graft amend` | lock, gate, `git add -A`, checks, `git commit --amend --no-edit`, no push; refuses a commit that is already on a remote, unless only on branches in `push.force_with_lease` |
| `graft release` | tag the pushed HEAD with the version, push the tag to every remote |
| `graft release --minor` / `--major` / `--version X.Y.Z` | choose the next tag (version mode `git-tag` only) |
| `graft version` | print the project version |
| `graft version bump [--minor\|--major]` | raise the version in the version file and its synced files without committing, for a script that folds the bump into its own commit (merging a work branch whose commits skipped it) |
| `graft version --describe` | the build identity for stamping binaries: `v1.4.0-3-gabc1234[-dirty]`, `1.4.0[-dirty]` in mode `file`, `v0.0.0-<commits>-g<hash>` without a tag |
| `graft init` | install the hooks and set `core.hooksPath`, create the `.env` file from `dotenv_template` when it is missing, install pinned tools that fail their check; safe to repeat |
| `graft check` | run the content checks on the index |
| `graft leaks <revision range>` | scan commits for the private names and terms of the user config (`leaks`) |
| `graft gate` | run the gate commands |
| `graft hook pre-commit` / `post-merge` | what the installed hooks call |
| `graft flags [status]` | is the flag raised: open journal events no exception mutes |
| `graft flags show <id>` | one event in full |
| `graft flags ack <id> --reason R` | close an open event with a reason |
| `graft flags mute <id> --reason R [--match S] [--all-envs]` | add an exception for it, then close it |
| `graft deploy [--redeploy] <target> [-- args]` | run the project's deploy script between graft's checks; `--redeploy` deploys the commit the target already runs |
| `graft deploy status <target>` / `logs <target> [--lines N] [--grep RE]` | the target's status and logs commands |
| `graft deploy check-head` | for deploy scripts: HEAD is still the commit being deployed |
| `graft <task> [-- args]` / `graft run <task> [-- args]` | run a task with its deps |
| `graft tasks` | every task, building blocks and deps included |
| `graft start` / `stop` / `restart [service]` | background services, all of them without a name |
| `graft logs <service> [--lines N] [-f]` | the end of a service's log, or its journal; `-f` follows until Ctrl-C |
| `graft stats [--since 7d] [--project <root>\|--all] [--task <name>]` | recorded resource use of tasks (see [Resource stats](#resource-stats)) |
| `graft status` | services, the test database, held locks |
| `graft locks` | who holds graft's locks |
| `graft testdb up` / `down` / `status` / `recreate` | the disposable test PostgreSQL |
| `graft tools [install]` | check the pinned tools; install the failing ones |
| `graft help` | the commands the project's `.graft.yaml` uses, its tasks (those with `desc`, with their arguments) and services |
| `graft --version` | print graft's own version |

### graft commit, step by step

1. Read the message (`-m`, repeatable, one paragraph each; `-F file`; `-F -`).
   An empty message is refused: there is no default subject. Warn when the
   hooks are not what `graft init` would install now: `core.hooksPath` not
   set to `hooks.dir` as written, or a shim missing or written for another
   `graft:` pin.
2. Take the repository lock. A second graft in the same repository (or in
   another worktree of it) waits for the first, up to `lock.timeout`, and says
   who holds the lock. The lock is an OS file lock and dies with its process.
   With `lock.scope: worktree` each worktree has its own commit lock, so
   agents in separate worktrees commit in parallel; `graft release` still
   takes the repository lock, which the main worktree's commits share.
3. Refuse if a merge, rebase, cherry-pick or revert is in progress, if HEAD is
   detached, or if there is nothing to commit.
4. Run the gate. A failure stops here, with the version untouched.
5. Bump the version file and every file synced with it, except on a branch
   in `version.skip_branches` (`--minor` and `--major` are refused there).
6. `git add -A`, then check the staged changes (secrets, binaries, sizes,
   version drift, and in a public repository private names in the added lines
   and the message). A failure restores the version files and stops.
7. Commit. The message is kept verbatim (`--cleanup=verbatim`): `$`, quotes,
   backticks and lines starting with `#` survive. When the branch name contains
   a work item key (`ticket.pattern`) and the message does not mention it yet,
   the key is added as a paragraph of its own, before a closing trailer block
   (`Co-Authored-By: ...`) so git still sees the trailers. graft adds no
   trailers itself.
8. Push to each remote in turn, with `--force-with-lease` on a branch in
   `push.force_with_lease`. A failing remote does not stop the others; the
   summary names every failure with the command to retry, and the exit status
   is non-zero.

### Hooks

`graft init` writes two small `sh` shims into `hooks.dir` (`.githooks` by
default) and points `core.hooksPath` at it with a relative path. Commit the
shims; every clone runs `graft init` once. The shim exits with an error and an
install hint when `graft` is not on `PATH`, so a missing binary never lets a
commit through unchecked.

- **pre-commit**: lets graft's own commits through (graft already ran
  everything). For a commit git's machinery drives (merge, rebase, cherry-pick,
  revert) it runs the checks and the gate. A hand-written `git commit` is
  refused with the graft commands this project's config allows.
- **post-merge**: after a real merge commit (one with a second parent) it runs
  the gate. A merge without text conflicts can still break the build; the hook
  says so and points at `graft amend`. Fast-forwards are not gated.

`graft init` rewrites shims an older graft wrote (the install hint follows
the project's `graft:` version). It refuses to overwrite hooks it did not
write and to repoint a
`core.hooksPath` that leads elsewhere. An absolute `core.hooksPath` to the same
directory is rewritten as relative. On Windows, keep the shims LF:
add `.githooks/* text eol=lf` to `.gitattributes`.

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

version:
  mode: file                    # required: file | git-tag | none
  file: VERSION                 # file mode; default VERSION, strictly MAJOR.MINOR.PATCH
  tag_prefix: v                 # default v
  tag_on_commit: false          # file mode: tag every commit v<version> and push the tag
  skip_branches: ['w/*']        # file mode: commits on these branches leave the version alone;
                                # the branch they are merged into bumps (graft version bump)
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
    dotenv: [API_KEY!, 'LOG_DIR?']  # only these .env keys reach the steps; KEY? optional, KEY! must not be empty
    dotenv_sets: [db]          # plus the keys of named sets
    env: {LOG_LEVEL: debug, API_URL: 'http://${HOST}:8080'}   # ${KEY}: environment, then .env
    run:
      - go test -count=1 {args} ./...      # graft test -- -run TestX
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

dotenv_sets:                   # key lists several tasks and services share
  db: [DB_HOST, DB_NAME, 'DB_POOL?']
```

- A step is a string (split into words, no shell; pipes, `$VAR` and
  redirects are errors), a list (argv as is), `{sh: "..."}`, `{task: name}`
  or `{service: action [name]}`. A string with `: ` in it is a map to YAML:
  quote the whole step.
- `{args}` receives the arguments after `--` verbatim: as separate words
  when it is a whole argv word, as the one argument inside a word, and
  shell-quoted in `sh` steps. `${KEY}` is expanded in the configured words
  only, never in what the user passed. A task without `{args}` refuses
  arguments; `graft help` shows which tasks take them. Inside `[...]`,
  quote it: `'{args}'`, `'--run={args}'`. Unquoted, YAML reads `{args}` as a
  map and cannot parse `--run={args}`; graft says so.
- Steps get graft's environment without git's hook variables and without
  the `.env` file: only the keys listed in `dotenv` and `dotenv_sets`, the
  `env` values, the test database DSN and `GRAFT_VERSION` are added.
  `GRAFT_VERSION` is `graft version --describe`; before the first commit
  there is none. In the gate of `graft commit` and `graft amend`,
  `GRAFT_COMMIT_MESSAGE_FILE` names a file with the message of the commit
  being made (the final text, work item key included), for checks that hold
  the message to what the change does; it is removed after the gate. Argv
  words see all of them as `${KEY}`.
- Before the first step, deps included, graft fits the arguments into the
  steps and checks every required key and `${KEY}` the task and all its deps
  need, listing everything missing at once. An optional `KEY?` set nowhere is
  left out; a `KEY!` set but empty is an error. Inside `[...]` it must be quoted:
  YAML does not take `KEY?` unquoted in a flow list.
- Locks are named read/write locks shared by all worktrees of the
  repository; a task inside a task under the same lock reuses it, and asking
  for write inside read is an error. Unknown deps and cycles are config
  errors.

### Resource stats

Every process a step starts (task steps, the gate, a deploy's deps and the
deploy script) is recorded when it ends: one JSON line in
`<user cache dir>/graft/stats.jsonl` (`$XDG_CACHE_HOME/graft` where set, else
`~/.cache/graft`), shared by every project and every graft on the machine.
A task gets a line of its own when it ends, covering everything it ran,
deps and inner tasks included.

```json
{"time":"2026-01-05T10:00:03.1+01:00","kind":"step","project":"/home/me/work/projectA","task":"lint","top":"commit","step":"shellcheck scripts/*.sh","wall_s":41.2,"user_s":38.9,"sys_s":2.4,"max_rss_mb":9123.4,"exit":0}
{"time":"2026-01-05T10:00:03.1+01:00","kind":"task","project":"/home/me/work/projectA","task":"lint","top":"commit","steps":2,"wall_s":44,"user_s":40.1,"sys_s":2.6,"max_rss_mb":9123.4,"exit":0}
```

- `top` is the graft command the task ran under (`commit`, `amend`, `gate`,
  `hook`, `deploy`, `start`), or the outermost task for `graft <task>`. The
  deploy script is recorded as task `deploy <target>`.
- `step` is the step as written in `.graft.yaml`, on one line, cut to 200
  characters. `exit` is -1 when there is no exit status: killed by a signal,
  or a task that failed outside its processes.
- `max_rss_mb` is the peak memory of **one** process: the step's own or that
  of a descendant it waited for, whichever is largest (`ru_maxrss` of
  `wait4`). It is not the sum over the process tree: eight linters in
  parallel at 1 GB each show 1 GB. Processes that are never waited for
  (daemons, detached children) are not counted, in memory or in CPU time.
  Windows reports no peak memory; the field is left out there.
- A line is one append-mode write, so concurrent grafts never split each
  other's lines. At 20 MB the file moves to `stats.jsonl.1`, replacing the
  older copy.
- A failure to record is not a failure of the task: graft warns once per run
  and goes on.

`graft stats` reports the current project for the last 7 days, one row per
task, the heaviest in memory first: runs, median and maximum wall time,
median CPU time (user + system), maximum peak memory, the share of failed
runs. `--since` takes days (`30d`) or a duration (`12h`), `--project` another
project's root, `--all` every project; `--task <name>` breaks one task down
into its steps.

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
  database: app_test
  user: app
  password: app                  # a throwaway local database
  settings: [fsync=off, synchronous_commit=off, full_page_writes=off]   # the default
  tmpfs: 2g                      # optional: keep the data in RAM (k, m or g; at least 64m)
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
settings or another `tmpfs` gets a warning; `graft testdb recreate` applies
the config.

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

## Tools

```yaml
tools:
  linter:
    go_install: example.com/linter/cmd/linter@v1.4.2   # pinned; @latest is refused
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
the project relies on. `graft tools` reports, `graft tools install` and
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
`go_install` path, the main module version the pinned one and, with `tags`,
the build tags exactly those. A mismatch is reported as such (`built from
example.com/migrator/cmd/migrator@v2.2.0, pinned v2.3.0`, `built without tags
postgres`), and so is a program with no Go build info. A copy built from a
local checkout carries `(devel)` or a `+dirty` version and never matches a
pin. `check` and `expect` go together; a `manual` tool needs both.

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
`"escaped \" \\ \n \$"` values, blank lines and `#` comment lines. A `#` after
an unquoted value is part of the value. Anything else is an error naming the
line.

## Flags

A project keeps a journal of events that need a human look (failed requests,
stalled jobs, unexpected log errors). `graft flags` shows whether the flag is
raised, that is, whether any open event is not muted by an exception, and closes
events with a reason. Every command takes `--env` (default
`flags.default_env`). Times are shown in UTC, with the zone named; a SQL
`timestamp` column without a time zone is read in the database session's
zone, as PostgreSQL itself reads it.

The exceptions file has one rule per line:

```
# env|class|substring|reason
*|http_failure|timeout calling provider|provider retries on its own
prod,test|log_error|disk almost full|disk alerts come from monitoring
local|job_stalled|*|jobs stall while the laptop sleeps
```

`env` is `*` or a comma-separated list of configured environments; `class` is
the event class; `substring` is matched literally and case-sensitively against
the subject (`*` mutes the whole class); the reason is required and may contain
`|`. A malformed line is an error, and `graft check` validates the staged file.
`graft flags mute` appends a rule for the current environment (`--all-envs`
for `*`), using the whole subject unless `--match` narrows it, and then closes
the event. Closing refuses an event that is not open, so another person's note
is never overwritten.

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
  targets:
    test:
      env: test                 # see envs
      deps: [db-smoke]          # tasks run first, as task deps: a quick check here
      run: [bash, deploy/deploy.sh, --env, test]   # argv, run here in the foreground
      version: "cat /opt/app/VERSION"              # prints what the target runs
      deployed_sha: {path: /opt/app/DEPLOYED_SHA, sudo: true}
      release_notes: {transition: Testing, from: [In Progress]}
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
      release_notes: {}

release_notes:
  jira:
    project_keys: [PROJ]        # required: SHA-256 is shaped like a key too
    comment: "Deployed to {target}, version {version}, commit {short}"   # default
    skip_statuses: [On Hold]    # leave these items alone: no note, no move
```

`graft deploy [--redeploy] <target> [-- args]`:

0. Check that every required key of `dotenv` and `dotenv_sets` and every
   `${KEY}` in `run` is set, and everything the `deps` tasks need, listing all
   that are not; nothing else happens before this passes.
1. The work tree must be clean; `git fetch <remote>`; HEAD must equal the remote
   branch.
2. With `requires`, the required target's `version` must print the version
   being deployed: a quick read-only check, so it fails before a long deps run.
3. With `deployed_sha`, read the commit the target runs. When it is the one
   being deployed, the deploy is refused (`test already runs <sha> <subject>;
   pass --redeploy to deploy it again`) before deps, sudo and the script;
   `--redeploy` deploys it again. With `deployed_sha.sudo`, this read runs
   `sudo` in the target environment before the confirmation prompt: on this
   machine it may ask for its own password there. A target without
   `deployed_sha` is deployed whatever it runs.
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
   `project_keys` item mentioned in the delivered commits (subjects and bodies),
   and with `transition`, a move to that status of the items in one of the
   `from` statuses (required with `transition`); an item elsewhere, paused or
   not started, gets the comment and keeps its status. A key mentioned in a
   commit does not make the item yours, so only open items of the owner of
   the Jira token get notes: not in a done-category status (Done, Won't do)
   or in `skip_statuses`, and assigned to the owner now or at some point in
   the item's history (an item handed over for review keeps its notes). Other
   items are left alone with a warning. Release notes never fail a deploy;
   problems are printed as warnings.

graft exits with the deploy script's exit status. Two deploys to the same
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
  the history the first release is **v0.1.0**. Tags that are not plain
  `vX.Y.Z` (such as `v1.0.0-rc1`) are ignored.
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
  refused.
- **Leaks**: with `leaks` in the user config, a commit to a repository that
  has a remote under `public_remotes` is checked for private names in the
  lines it adds and in its message. The lists live in the user config, never
  in the public repository:
  - **names**: camel-case functions, methods and types of at least
    `min_name_length` characters declared in `private_sources` (parts of test
    names included, the test's own name not). A name the public Go code in
    GOROOT and the module cache also uses is common and dropped; modules under
    `public_remotes` and the private sources' own modules do not count as
    public code. The first run indexes the module cache, later runs only the
    new modules (`<user cache dir>/graft/leaks`). A name the repository
    declares in its own non-test Go code is its vocabulary; a mention in
    tests, fixtures, comments, docs or the message is a finding.
  - **terms**: every match of a `terms_file` regexp.

  A finding names the file, line and the name or term. A line containing
  `graft:leak-ok <reason>` passes; `leaks.allow` passes a name or term
  everywhere. `graft leaks <revision range>` runs the same scan over existing
  commits, each against its parent, to audit a history.

## Development

graft builds, tests and commits itself through its own `.graft.yaml`:

```sh
go run . smoke     # the gate: gofmt check, vet (host and windows), all tests
go run . all       # smoke + race + glint
go run . install   # ~/bin/graft
go run . commit -m "..."
```

Tests of the SQL adapter need PostgreSQL and psql, the test database tests
need docker: the `test` task brings up the test database and passes its DSN
in `GRAFT_TEST_PG_DSN`. Plain `go test` skips those tests and says so.

## License

MIT
