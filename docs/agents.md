# Working with graft

This guide describes the graft version that printed it. Project configuration
and task descriptions decide which operations are available.

## Tasks

Use `graft help` to see commands and the project's tasks and descriptions.
Use `graft tasks` to list building blocks too. Read `.graft.yaml` for details.
Run `graft <task> -- args` to pass arguments to a task.
Use `graft -C <directory> <task>` to select a directory before the command.
If the project lacks a repeatable task, add one to `.graft.yaml` with a desc.
For a one-off command use `graft exec --heavy -- <cmd>` when it is heavy.

## Commit

`graft commit` stages changes, runs the gate and checks the staged content.
It can add a ticket key from the branch, commit and push to every remote.
Read a refusal and its hint, then fix the cause before trying again.
graft commit has no flag to skip a check. Flags that relax a safeguard elsewhere (deploy --unapproved, --past-freeze) are for the user's explicit word only.

## Merge

Run `graft merge <branch>` on the trunk to build a checked candidate in a
temporary worktree, push it to the primary remote and mirrors, then install
it with a fast-forward. Rebase is the default; `--mode merge` preserves the
source commits. Conflicts outside the configured version file stop the
operation. `merge.deps` runs after the gate and content checks; skipping it
requires `--no-deps --reason <text>`. The repository lock stays held through
the checks, so `graft commit` waits. The source branch is not changed.

## Heavy

Heavy work shares a machine queue. The queue supplies the thread count through
`{threads}` and `GRAFT_THREADS`; use the count in parallel build and test tools.
On supported Linux systems heavy scopes have a memory ceiling; the default is
34% of RAM. Configure a task's `memory_max` or exec's `--memory-max` as needed.
A project can send heavy work to `remote_heavy`; `graft exec --heavy --local -- <cmd>`
keeps that command on this machine. The remote receives selected environment
variables, not the caller's entire environment; use `--env` to pass another.

## Tests

Use the project's test task and pass runner arguments after `--`.
Tasks marked `tests: go` read Go's JSON events and report skipped tests.
For supported direct Go test commands, graft checks that each `-run` alternative
matched a test; complex expressions and some command forms are not checked.
Tasks marked `tests: jest` or `tests: playwright` read the JSON report at
`GRAFT_TEST_REPORT`. Configure the runner to write that file.
A task with `cache: tree` can reuse a pass on the same inputs. Use
`graft test --no-cache` to run it again when test is the task name.

## Stats and limits

`graft stats` shows recorded task resource use and timings.
`graft stats --tests` reports slow, skipped and flaky tests.
`graft stats --failures` shows recent failures and their captured reasons.
Filter reports with options shown by `graft stats -h`.
Heavy memory ceilings depend on platform and available system support;
read the task output when graft reports that it cannot set one.

## Deploy

Read the target's configuration and `graft deploy -h` before deploying.
`graft deploy check <target>` reports refusals without deploying.
Deploy checks required environment, a clean and published tree, required
versions, deployed state and configured release, window and integration rules.
It then runs dependencies, obtains configured confirmation, runs the script,
checks the tree and HEAD again, and records the deploy and release notes.
`graft deploy freeze <target>` shows an active freeze; supplying a version
can create one. `graft deploy thaw <target> --reason reason` ends it.
`--redeploy`, `--unapproved` and `--past-freeze` change safeguards or allow
exceptions: use them only on the user's word.

## Worktrees

`graft worktree add <branch>` creates a worktree with configured setup.
`graft worktree sync` rebases its work onto the fresh trunk and preserves
uncommitted changes; conflicts can require manual resolution.
`graft worktree check <path>` reports whether removing a worktree loses data.
`graft worktree clean` reports candidates; `--apply` removes eligible trees
and branches, including eligible remote branches. Review before applying.
Read-only inspection and the clean report are safe to run without asking;
coordinate changes to branches and removal with the user's instructions.

## Leaks

Keep private names and terms in the user config, not in a public repository.
The global pre-push hook checks outgoing commits when leaks are configured.
`graft leaks <revision>` scans existing commits using those rules.
Fix findings at their source and check both changes and commit messages.
