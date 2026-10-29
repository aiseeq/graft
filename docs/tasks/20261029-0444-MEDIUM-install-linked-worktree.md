# install: отказ Go-бинарю без ревизии из связанного worktree

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Находка админа пространства (2026-10-29): `{install: …}` из связанного worktree ставит Go-бинарь без vcs.* — свежесть установленного потом не проверить (`go version -m`, проверка свежести бинарей). Причина: cmd/go (go1.26, internal/vcs/vcs.go, vcsGit RootNames `.git` isDir: true) признаёт корнем git только папку .git; в linked worktree .git — файл, vcs молча не ставится даже с -buildvcs=true.
- Сейчас prepareInstall (internal/tasks/tasks.go) у Go-бинаря без vcs.revision проверяет только чистоту дерева и ставит с ревизией HEAD.
- Сделать: Go-бинарь (buildinfo читается) без vcs.revision, собранный в связанном worktree (`git rev-parse --git-dir` ≠ `--git-common-dir`), — отказ install до записи, текст: в связанном worktree go build не ставит ревизию в бинарь, ставить из основного дерева (его путь — из `git worktree list`) после влития. Не-Go файл и основное дерево — как раньше.
- README, раздел install step: строка про отказ.

## Приёмка
- accept_install_worktree_test.go (scratchpad accept/wtinstall): красный на main, зелёный после; `go run . all`.

## Журнал
- 2026-10-29 04:44 открыт — graft-master
- 2026-10-29 04:59 в работе — graft-master
- 2026-10-29 04:59 note — graft-master: боец Luna 1029-0459-install-linked-worktree, worktree ~/.cache/worktrees/graft/install-linked-worktree, приёмка accept/wtinstall.sh (красная на main 04:58: install из linked worktree прошёл); решение: отказ, а не сборка во временном клоне — из worktree ставится невлитый код, ставить надо из основного дерева после влития
- 2026-10-29 05:06 note — luna: luna: Refuse Go binaries without vcs.revision from linked worktrees and document the install behavior.
- 2026-10-29 05:06 note — graft-master: ревью мастера: дифф верен (GitDir/CommonDir абсолютные — основное дерево не задето); приёмочный тест перенесён в install_integration_test.go (TestInstallStepRefusesLinkedWorktreeGoBinary), имя файла в ошибке разбора worktree list; TestInstall* зелёные
