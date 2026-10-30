# graft stats --in: отбор задачи по родителю

Статус: открыт

## Что сделать
Запрос git-admin 30.10 ~16:55 CET (замер glint мастером glint): задача glint у потребителей идёт внутри smoke, deploy, analyze; `graft stats --task glint` смешивает все. Поле `in` (internal/stats/stats.go Record.In) уже пишется.

- Флаг `--in <задача или deploy>` у `graft stats`: берёт только записи задачи (`kind=task`), у которых `in` равно значению; и в отчёте (`--task T --in P`), и в `--value` (wall-p50, wall-p95).
- `--in` без `--task` — ошибка использования (код 2), как прочие несовместимые флаги `--value` (main.go validateStatsValue).
- Справка help.go, README (раздел stats, рядом с `--value`).

## Приёмка
- Тест internal/stats на синтетике: glint с in=smoke, in=deploy и без in — `--in smoke` берёт только первую; wall-p50 по ней.
- Интеграционный тест в stats_integration_test.go: `graft stats --task glint --in smoke --value wall-p50` печатает число по записям только из smoke.
- `go run . all` зелёный.

## Журнал
- 2026-10-30 17:03 открыт — graft-master
- 2026-10-30 17:04 note — graft-master: graft-master: боец Luna (фича-инструмента): бриф /home/aisee/.cache/worktrees/graft-measure/brief-stats-in.md, приёмка /home/aisee/.cache/worktrees/graft-measure/stats-in-accept.sh, дерево /home/aisee/.cache/worktrees/graft/feat-stats-in
- 2026-10-30 17:28 note — graft-master: graft-master: ревью Terra 1030-1711: P2 --tests с --in и P2 потеря шагов исправлены в 77c78f3; повтор: шаги относятся по процессу graft — процесс, запустивший задачу в двух родителях, покажет шаги обоих; принято ограничением (у строк шагов нет in), описано в README и комментарии ranIn.
