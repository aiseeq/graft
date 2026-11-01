# loadgate: мера writeback из akit v1.22.15 (Dirty+Writeback ≥ max_dirty_mb), подсказка ключа

Статус: на проверке
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
- 2026-11-01 15:24 note — graft-master: Коммиты 4e91154 (мера writeback, подсказка, calm meminfo в подделке), 542ef26 (akit v1.22.16 по поправке akit-master: порог по умолчанию — vm.dirty_background_* машины; в подделке dirty_background_bytes 64 MiB). Первый гейт 542ef26 упал: shm 61/64 МБ в graft-test-pg (сегменты DSM за 13 дней без клиентов) — docker restart, повтор зелёный. go run . all — зелёный. Релиз v2.73.0, install ноут+pc (sha 542ef26).
- 2026-11-01 15:24 на проверке — graft-master — на независимую проверку
- 2026-11-01 15:44 note — graft-master: Проверка Terra: не принято — команды не запускались (песочница только чтение). Уточнение приёмки: akit v1.22.16 (поправка akit-master), подсказка «(pressure.yaml max_dirty_mb, else vm.dirty_background_bytes or _ratio)». Замечание о внутренних именах в docs/tasks — снято решением public-repo-names (28.10: имена личных репо не утечка; рабочих имён grep не нашёл). Повторная проверка — суб-агентом без песочницы.
