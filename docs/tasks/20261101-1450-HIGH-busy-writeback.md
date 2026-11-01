# loadgate: мера writeback из akit v1.22.15 (Dirty+Writeback ≥ max_dirty_mb), подсказка ключа

Статус: в работе
Исполнитель: graft-master

## Что сделать
Задание лида через akit-master 01.11 (тикет лида laptop-freeze-1031, akit docs/tasks/20261101-1437-HIGH-busy-writeback.md): akit v1.22.15 судит в pressure.Rule меру writeback (Dirty+Writeback из /proc/meminfo ≥ max_dirty_mb, умолч. 64 MiB). graft на v1.22.14 её не видит.

План:
- akit → v1.22.15 (graft commit поднимет сам по modules.latest);
- loadgate.measures: writeback → WhyWriteback "writeback", подсказка «pressure.yaml max_dirty_mb»; тест в TestBusyDiskAndSwapHoldTasks;
- skip(): ErrNoMemInfo — строка «not checked» (default) — проверить;
- graft all, коммит, install.

Чем проверить: `go test ./internal/loadgate -run TestBusyDiskAndSwapHoldTasks/writeback` зелёный; строка ожидания содержит «(pressure.yaml max_dirty_mb)»; `go list -m github.com/aiseeq/akit` = v1.22.15; установленный graft — `graft version`.

## Приёмка

## Журнал
- 2026-11-01 14:50 открыт — graft-master
- 2026-11-01 14:50 в работе — graft-master
