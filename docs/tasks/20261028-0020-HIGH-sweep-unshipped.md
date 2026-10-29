# sweep says an item's work is on the target when it ships nothing there

Статус: готово
Исполнитель: graft-master

## Вход
- Проблема: a peer saw `sweep prod: … moved to Done: its work of the last 30
  days is on prod (commits: 4)` for an item whose 4 commits are no ancestors
  of the prod commit.
- Метод: measured — the commits change only `.graft.yaml` (in
  approved_release.ignore_paths) and the version file; `landed`
  (internal/app/sweep.go) counts such a commit on the target by design since
  75257b8 (it ships nothing). The move is right, the line is wrong.
- Дешевле: none.
- Инструмент: sweep.
- Цена: an hour.
- Если не делать: agents read the line as a false move and undo it.

## Что сделать
1. `sweptCommit` tells a commit on the target from one that ships nothing
   there (lands only through ignore_paths).
2. The move line and the listed lines say `ships nothing to <target>
   (approved_release.ignore_paths)` for those, counted apart.

## Приёмка
- Sweep test: an item whose commits change only ignored files moves with the
  new words; an item with real work on the target keeps `is on`.

## Журнал
- 2026-10-28 00:20 открыт — graft-master
- 2026-10-28 00:21 cause measured (git show --stat of the 4 commits); answered the peer
- 2026-10-28 00:26 shipsNothing in sweptCommit, onTargetWords; TestDeploySweepByContent PROJ-85; README; committing
- 2026-10-28 01:00 на проверке — graft-master — installed v2.44.0; the reporting peer to confirm on its next prod sweep
- 2026-10-28 13:45 note — graft-master: verify subagent 13:35: PASS 3/3; found wording defect (commit with real+ignored files, real part on target -> said ships nothing). Fixed a093fe4 (changesOnlyIgnored), test PROJ-86/87 red before; v2.48.2. Re-verify subagent 13:5x
- 2026-10-28 13:50 готово — verify-subagent: subagent 13:35+13:5x on v2.48.2: abc0bbc logic PASS, TestDeploySweep ok, revert red (PROJ-85); wording defect fixed a093fe4, revert red on PROJ-86, PROJ-87 mixed line present
