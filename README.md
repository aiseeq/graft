# graft

One commit command for every repository you work in. `graft commit` runs the
project's gate, bumps the version, stages everything, scans the staged changes
for secrets and oversized files, commits with the work item key taken from the
branch name, and pushes to every remote. Hooks installed by `graft init` keep
plain `git commit` out while letting git's own merges, rebases and
cherry-picks through the same gate.

What differs between projects lives in `.graft.yaml` at the repository root.
graft is a single Go binary with no shell dependency in its core, so it works the
same on Linux, macOS and Windows (Git for Windows).

## Install

```sh
go install github.com/aiseeq/graft@latest
```

or, from a checkout, `make install` (atomically replaces `~/bin/graft`).
Requires git 2.31 or newer.

## Commands

| Command | What it does |
|---|---|
| `graft commit -m "fix: ..."` | lock, gate, bump patch, `git add -A`, checks, commit, push to every remote |
| `graft commit --minor` / `--major` | same with a minor / major bump (version mode `file` only) |
| `graft commit -F msg.txt` / `-F -` | message from a file / from stdin, byte for byte |
| `graft amend` | lock, gate, `git add -A`, checks, `git commit --amend --no-edit`; refuses a commit that is already on a remote |
| `graft release` | tag the pushed HEAD with the version, push the tag to every remote |
| `graft release --minor` / `--major` / `--version X.Y.Z` | choose the next tag (version mode `git-tag` only) |
| `graft version` | print the project version |
| `graft init` | install the hooks and set `core.hooksPath`; safe to repeat |
| `graft check` | run the content checks on the index |
| `graft gate` | run the gate commands |
| `graft hook pre-commit` / `post-merge` | what the installed hooks call |
| `graft flags [status]` | is the flag raised: open journal events no exception mutes |
| `graft flags show <id>` | one event in full |
| `graft flags ack <id> --reason R` | close an open event with a reason |
| `graft flags mute <id> --reason R [--match S] [--all-envs]` | add an exception for it, then close it |
| `graft --version` | print graft's own version |

### graft commit, step by step

1. Read the message (`-m`, repeatable, one paragraph each; `-F file`; `-F -`).
   An empty message is refused: there is no default subject.
2. Take the repository lock. A second graft in the same repository (or in
   another worktree of it) waits for the first, up to `lock.timeout`, and says
   who holds the lock. The lock is an OS file lock and dies with its process.
3. Refuse if a merge, rebase, cherry-pick or revert is in progress, if HEAD is
   detached, or if there is nothing to commit.
4. Run the gate. A failure stops here, with the version untouched.
5. Bump the version file and every file synced with it.
6. `git add -A`, then check the staged changes (secrets, binaries, sizes,
   version drift). A failure restores the version files and stops.
7. Commit. The message is kept verbatim (`--cleanup=verbatim`): `$`, quotes,
   backticks and lines starting with `#` survive. When the branch name contains
   a work item key (`ticket.pattern`) and the message does not mention it yet,
   the key is added as a paragraph of its own, before a closing trailer block
   (`Co-Authored-By: ...`) so git still sees the trailers. graft adds no
   trailers itself.
8. Push to each remote in turn. A failing remote does not stop the others; the
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
  refused with the graft command to use instead.
- **post-merge**: after a real merge commit (one with a second parent) it runs
  the gate. A merge without text conflicts can still break the build; the hook
  says so and points at `graft amend`. Fast-forwards are not gated.

`graft init` refuses to overwrite hooks it did not write and to repoint a
`core.hooksPath` that leads elsewhere. An absolute `core.hooksPath` to the same
directory is rewritten as relative. On Windows, keep the shims LF:
add `.githooks/* text eol=lf` to `.gitattributes`.

## .graft.yaml

```yaml
schema: 1                       # required

# Commands run before every commit, in order, stopping at the first failure.
# A string is split into words by shell quoting rules but is NOT run by a
# shell: pipes, &&, redirects, $VAR, globs and VAR=value prefixes are
# rejected. Use the list form for exact argv, or call a script.
gate:
  - make smoke
  - [go, test, -short, ./...]
  - [sh, -c, 'go vet ./... && go test ./...']

version:
  mode: file                    # required: file | git-tag | none
  file: VERSION                 # file mode; default VERSION, strictly MAJOR.MINOR.PATCH
  tag_prefix: v                 # default v
  tag_on_commit: false          # file mode: tag every commit v<version> and push the tag
  sync:                         # file mode: files that repeat the version
    - path: web/package.json
      format: json              # value at the key path; formatting is preserved
      key: [version]
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
```

Unknown keys are errors, so a typo cannot silently switch a check off. All
paths are relative to the repository root and use forward slashes.
Exception paths are globs: `*` and `?` stay within one directory, `**` spans
directories.

## Environments and .env

```yaml
dotenv: .env                    # default; read key by key, never sourced or exported whole
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
shell.

The `.env` parser accepts `KEY=value`, `export KEY=value`, `'literal'` and
`"escaped \" \\ \n \$"` values, blank lines and `#` comment lines. A `#` after
an unquoted value is part of the value. Anything else is an error naming the
line.

## Flags

A project keeps a journal of events that need a human look (failed requests,
stalled jobs, unexpected log errors). `graft flags` shows whether the flag is
raised, that is, whether any open event is not muted by an exception, and closes
events with a reason. Every command takes `--env` (default
`flags.default_env`).

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
      prod:
        # run in the environment (over ssh); {method}, {path}, {body_b64} are shell-quoted
        command: "API_METHOD={method} API_PATH={path} API_BODY_B64={body_b64} sh -s"
        stdin_file: scripts/api-request.sh    # fed to the command
```

Listing follows `limit`/`offset` pages until `total`. Fields are dotted paths
into the response; times are RFC 3339. A direct request that fails shows the
response body; a command transport returns the response on stdout.

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

## Development

```sh
make smoke    # gofmt check, vet (host and windows), all tests
make check    # smoke + race + glint
make install  # ~/bin/graft
```

Tests of the SQL adapter need PostgreSQL and psql: `make test` starts a
disposable container (`make pg-up`, docker) and passes its DSN in
`GRAFT_TEST_PG_DSN`. Without it, `go test` skips those tests and says so.

graft commits itself: `go run . commit -m "..."`.

## License

MIT
