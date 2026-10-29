# install: отказ Go-бинарю без ревизии из связанного worktree

Статус: открыт

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
