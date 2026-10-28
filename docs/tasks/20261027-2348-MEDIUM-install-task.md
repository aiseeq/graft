# One graft task installs a project binary into ~/bin

Статус: ждёт
Ждёт: project masters switched their installs to the step (the space admin reports); graft's own go run . install passes once the pilot tree is clean
Исполнитель: graft-master

## Вход
- Проблема: projects install their binaries into ~/bin each its own way (an
  `install` task running sh, a script of the project, a Makefile target, none).
- Метод: one form of the task, graft-run, differences only where a project
  needs them.
- Дешевле: a README recipe alone, no graft change; to decide after the census.
- Инструмент: graft tasks (`.graft.yaml`), README.
- Цена: about a day with the census and the handover to project agents.
- Если не делать: every project agent keeps its own install, atomicity and
  rebuild rules differ.

## Что сделать
1. Census of every ~/git project: how it installs, what it installs, extras.
2. One form (a built-in or a documented task shape), in graft and README.
3. Edits to each project's `.graft.yaml` go to that project's agent.

## Перепись (28.10 02:40, по `go version -m` установленных бинарей)
The full census with project names is kept outside this public repository.
It goes to the space admin with the handover. Here it is by kind:
- 3 projects use their own install.sh. It refuses a dirty tree, builds to
  `.next`, mv's it into place, refuses vcs.modified and prints the revision.
  One of them also skips a binary already at HEAD. Another installs one
  binary into a second directory, and only while nothing runs it.
- 3 projects use a graft task `install` with sh: cp or mktemp, then mv. None
  checks for uncommitted changes. graft runs its pilot first; one project
  also writes a config file.
- 1 project uses a Makefile `install`, and its installed binary is
  vcs.modified=true.
- 1 project is installed by hand, into two directories.
- 2 projects use a symlink into the repository's own bin/.
- 1 project installs shell scripts as symlinks. It is out of scope: these
  are not built binaries.
- Binaries from older builds still sit in place: one project's tools run
  versions v1.0.0 to v1.5.1 side by side.

## Форма (решение мастера 28.10)
Step kind `install:` in a graft task — the project's build stays its own
task (deps), the step only installs:
```yaml
install:
  deps: [build]
  run:
    - install: bin/glint                        # into tools.bin_dir of the user config, else go install's GOBIN
    - install: bin/*                            # globs; every match
    - {install: bin/agent, to: ~/data/bin}      # to: absolute or ~/
```
For each file:
1. It refuses a file built from uncommitted changes. When the file has Go
   build info, it refuses vcs.modified=true, and a vcs.revision other than
   HEAD (a stale build). When the file has no Go build info or no revision,
   the work tree must be clean.
2. A destination with the same bytes is left alone: `unchanged`.
3. The file is written atomically: a temp file in the destination dir, mode
   0755, rename. On Windows, when the rename fails (a running exe), the old
   file is moved aside to `.<name>.old-<ns>` first. The old file is removed
   when nothing runs it any more, by this install or a later one.
4. Each file prints one line `graft: install: <name> <rev7> -> <path>`.
5. The step counts as a side effect: the task cache never skips it.

Project extras stay as their own sh steps around it: graft's pilot, a
config file, a check that nothing runs the old binary.

Rejected: a built-in `graft install` command, because tasks named install
already exist in 4 projects and it would conflict with them. Rejected: a
README recipe alone, because it would not stop installs from a dirty tree
(the Makefile project).

Rollout order: the installed graft must know the step before graft's own
`.graft.yaml` uses it, because the hooks parse the config with
~/bin/graft. So there are two commits: first the step, then release and
install, then graft's install task switches to the step.

## Приёмка
- Every project that installs a binary does it by the one form; the rest
  named with the reason.

## Журнал
- 2026-10-27 23:48 открыт — graft-master
- 2026-10-28 02:33 в работе — graft-master
- 2026-10-28 02:33 note — graft-master: перепись и форма записаны; реализация — боец Luna (класс фича-инструмента, маршрут luna), приёмочный тест пишет мастер
- 2026-10-28 02:34 note — graft-master: боец Luna 1028-0234-feat-install-step, branch feat/install-step, acceptance: sha256 of install_integration_test.go + go run . gate
- 2026-10-28 02:52 note — graft-master: worker passed acceptance (16 min). Master review fixed 4 things: on Windows the swap failed when removing the running `.old`; a 2-key map without `install` got the install error; tools.Install read the user config twice; the README bullet. Tests TestInstallStep|TestTask|TestTools green. The .graft.yaml switch is set aside for the second commit.
- 2026-10-28 02:59 note — graft-master: 70724d7 step committed; v2.45.0 tagged and installed (~/bin/graft vcs.revision=70724d7, modified=false). 0a5c73a: graft's install task uses the step. The dogfood install is refused by the pilot: the pilot project's uncommitted work has its own glint High finding. That is not graft — the same code passed the pilot at v2.45.0. Retry go run . install when the pilot tree is clean. Next: handovers to consumer masters.
- 2026-10-28 02:59 note — graft-master: 03:01 handover with the per-project list sent to the space admin (it distributes to the project masters); worker outcome recorded (merged)
- 2026-10-28 02:59 ждёт — graft-master — project masters switched their installs to the step (the space admin reports); graft's own go run . install passes once the pilot tree is clean
