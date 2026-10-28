# leaks check counts moved lines as added

Статус: открыт

## Вход
- glint-master 28.10: moving pkg/rules/patterns/X_test.go → pkg/rules/patterns/<group>/X_test.go fails graft commit with 621 leak findings; every line is removed verbatim by the same commit. Blocks gocache-growth.
- Cause: internal/leaks/scan.go diffArgs --no-renames --diff-filter=AMT; ParseDiff takes every added line.

## Что сделать
1. ParseDiff: an added line whose exact text the same diff removes (multiset, any file) is a move, not added. Deleted files in the diff (AMTD).
2. Same for CommitLines (pushed-history scan).
3. README leaks section: moves do not count.

## Приёмка
- Unit test: a file moved with a private term stays clean; a term added beside the move is found; a line added twice but removed once is found once.
- glint-master's tree patterns-split2 commits.

## Журнал
- 2026-10-28 13:33 открыт — graft-master
