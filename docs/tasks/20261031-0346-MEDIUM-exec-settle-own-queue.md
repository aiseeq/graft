# exec --settle держит только следующий --settle, не всю очередь

Статус: готово
Исполнитель: graft-master

## Что сделать
`graft exec --heavy --settle D` (допуск бойцов агентного инструмента) сейчас держит общий load.lock D после старта (internal/loadgate/loadgate.go Wait/settle): за 30.10 155 допусков по 60 с = 2.6 ч удержания, 2231 с из 10 536 с ожидания гейтов (21%), 45 гейтов из 260 (пункт 8 stats-design-review); 31.10 03:29 — 8 талонов, из них 5 гейтов, стояли за допуском при runnable 5 из 22.
Нужно: общая очередь держится обычным load.settle; сверх него удержание D ждёт только следующий `--settle` (свой лок рядом с load.lock), гейты и прочие тяжёлые задачи проходят. Согласовано с хозяином потребителя (письмо 31.10 03:41: «удержание --settle ждёт только следующий допуск бойца, гейты и прочие задачи очереди проходят»).

## Приёмка
- Тесты: при живом `exec --settle 60s` следующий `exec --heavy` без --settle стартует через load.settle, а следующий `exec --heavy --settle` ждёт удержания первого; конец команды отпускает оба лока.
- `graft gate` зелёный; README (абзац --settle) и help.
- Версия graft с правкой — хозяину потребителя, он снимет лишнее у себя.

## Журнал
- 2026-10-31 03:46 открыт — graft-master
- 2026-10-31 03:46 в работе — graft-master
- 2026-10-31 03:46 note — graft-master: 03:50 боец Luna 1031-0346-feat-exec-settle-own-queue, бриф /home/aisee/.cache/worktrees/graft-measure/brief-exec-settle-own-queue.md
- 2026-10-31 10:23 note — graft-master: 10:5x в v2.64.0 (0b99f15), установлено da94314; all на 0b99f15 зелёный (scratchpad all-0b99f15.log)
- 2026-10-31 10:23 note — graft-master: поправка: время релиза 10:20, не 10:5x
- 2026-10-31 10:23 на проверке — graft-master — приёмка: go test -count=1 -run TestExecSettle -v .; go test -count=1 ./internal/loadgate/
- 2026-10-31 10:31 готово — llmark-master (session graft-master): llmark-master 31.10 10:23 CET, v2.64.0, живая проба на ноуте: держатель exec --heavy --local --settle 40s -- sleep 40; обычный exec --heavy ждал 19 с (прежний держатель + load.settle 10 с, не 40 с), --settle 1s — 30 с за держателем; graft locks показал load и load.settle
