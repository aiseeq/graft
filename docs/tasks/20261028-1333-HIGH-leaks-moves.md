# leaks check counts moved lines as added

Статус: готово
Исполнитель: graft-master

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
- 2026-10-28 13:45 note — graft-master: fixed 2cc8228+9bb1c2b, v2.48.2 installed laptop+pc (sha 9e578dd6); graft check on glint patterns-split2: v2.48.1 fails, fix passes; glint-master told. Independent verify: subagent 13:5x
- 2026-10-28 13:50 готово — verify-subagent: subagent 13:5x, ~/bin/graft check e2e with public remote + terms: git mv clean; move + new term found once; duplicated moved line found once; "-- " removal hunks parse; no new line passes (moved requires identical line in HEAD; a HEAD line never scanned is published with HEAD anyway). glint-master committed the split on v2.48.2.
