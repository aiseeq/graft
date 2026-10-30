# graft stats --value: одно число времени задачи для ночного обхода perf

Статус: готово
Исполнитель: luna

## Вход
- Мастер соседнего проекта 30.10 ~14:45 CET (письмо graft-master): проекты пишут свои копии jq по ~/.cache/graft/stats.jsonl* для строк `команда` в ~/.claude/improve/state/perf/<проект>.tsv (FORMAT.md там же): две копии в рабочих проектах (медиана wall_s − wait_s успешных прогонов за 24 ч, код 3 при < 3), третья — моя /home/aisee/.claude/improve/state/perf/graft-perf/task-p95.sh (p95, очередь).
- Одна логика на действие (dev-process): одно место — `graft stats`.

## Что сделать
- `graft stats --value wall-p50|wall-p95|wait-p95 [--task T] [--since 24h] [--min-runs N] [--project root|--all]`: stdout — одно число секунд, код 0; меньше N прогонов (умолчание 3) — пусто, код 3.
- wall-*: собственное время (wall_s − wait_s) успешных (exit 0) прогонов задачи T; wait-p95: wait_s прогонов (без --task — внешних задач процесса graft, без `in`).
- Проект: все его деревья (основное и worktree) — по `profile` «<имя>:…», для записей без profile — путь проекта или его worktree.
- README, тесты на синтетике.

## Приёмка
- Тесты пакета stats и app; `go run . all` зелёный.
- Сверка с jq на настоящем журнале: команда и эталонный jq дают одно число на 3 задачах.

## Журнал
- 2026-10-30 14:38 открыт — graft-master
- 2026-10-30 14:39 в работе — graft-master
- 2026-10-30 14:39 note — graft-master: graft-master: боец Luna (фича-инструмента), бриф /home/aisee/.cache/worktrees/graft-measure/brief-stats-value.md, приёмка stats-value-accept.sh, дерево /home/aisee/.cache/worktrees/graft/feat-stats-value
- 2026-10-30 15:09 note — graft-master: graft-master: боец 1030-1439 принят приёмкой; моё ревью: верно; правил справку (двойной --task) и README (деревья проекта); коммит 26a6298 в feat/stats-value. Округление ранга p95 сверил с целочисленным для n до 100000 — расхождений нет. Профиль без fallback по пути при известном имени: записей без profile в журнале 0 — не важно. Независимое ревью — Terra.
- 2026-10-30 15:22 готово — graft-master: независимое ревью Terra 1030-1509: числа = её jq на копии журнала (gate wall-p50 23.666, test wall-p95 53.074, wait-p95 120.902, gate проекта из worktree 85.318), обычный отчёт побайтно как в main; 3 находки (записи без profile, пустой --value, формула в README) исправлены e6a0e58 с тестами; go run . all exit 0 на e6a0e58
