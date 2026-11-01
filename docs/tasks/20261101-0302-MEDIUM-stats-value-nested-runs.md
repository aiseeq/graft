# stats --value считает прогоны пилота (вложенные в задачу другого проекта) своими

Статус: в работе
Исполнитель: graft-master

## Вход
- glint-master 01.11 02:50 CET: `graft stats --project <пилот> --task glint --since 24h --value wall-p50` = 7.197, хотя в пилоте glint за сутки не запускался; в медиане 13 прогонов пилота /tmp/claude-tmp/graft-pilot.*/pilot (profile <пилот>:glint, 31.10 04:48–21:55). Обход perf Сна принял их за регресс пилота ×2.78.
- Причина (проверено): internal/stats/value.go projectRecord — запись с profile совпадает по префиксу `<repo>:`, дерево не проверяется; копия — git worktree проекта-пилота (scripts/pilot-gate.sh), profile верный.
- Цепочка parent: запись пилота glint → parent 7db1677393ca00c9 = graft:pilot-gate в /home/aisee/git/graft → parent graft:build. Вложенный прогон помечен уже сейчас.

## Что сделать
- В Value: запись, у которой предок по цепочке parent (run→record) относится к другому project, — не прогон этого проекта (это часть задачи внешнего проекта). Предок вне журнала (ротация) — цепочка обрывается, запись считается по своему profile/дереву.
- Проверить остальные отчёты stats, отбирающие по profile/project (rowKey, ranIn, tests.go, reruns.go) — где ещё вложенные прогоны пилота попадают в чужой проект.
- История ядер load.cores.json (appendTaskAndHistory) копит профиль <пилот>:glint и с пилота — оценить, нужно ли отделять (холодный кэш); решение строкой в журнал.

## Приёмка
- Тест в internal/stats: записи project=X profile=p:glint с parent → run в project=G — не входят в Value для p; без parent и с parent того же проекта — входят. Красный на 1363fd0.
- `graft stats --project <пилот> --task glint --since 48h --value wall-p50` на новом бинаре не содержит прогонов graft-pilot (сверка числа runs с jq по stats.jsonl).
- go run . all зелёный.

## Журнал
- 2026-11-01 03:02 открыт — graft-master
- 2026-11-01 03:02 в работе — graft-master
- 2026-11-01 03:02 note — graft-master: Боец Luna 1101-0302-stats-value-nested-runs: бриф /home/aisee/.cache/worktrees/graft-measure/brief-stats-value-nested.md, приёмка /home/aisee/.cache/worktrees/graft-measure/accept-stats-value-nested.sh (на main красная: graft 4.301 по 32 прогонам с пилотом, jq без пилота 2.774 по 19). История ядер — решу после ревью.
