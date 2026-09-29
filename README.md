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

graft commits itself: `go run . commit -m "..."`.

## License

MIT
