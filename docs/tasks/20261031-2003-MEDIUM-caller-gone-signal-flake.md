# TestSignalStopsTheCommand: гонка маркера со временем на нагруженной машине

Статус: открыт

## Что сделать

## Приёмка
- Тест не зависит от времени: ребёнок пишет свой pid и спит долго; после выхода graft проверяется, что процесс ребёнка мёртв (kill(pid,0) → ESRCH, ожидание с запасом ≥ 5 с только как потолок, не как окно).
- Красный до правки воспроизводится искусственной задержкой (например, `stress`/`GOMAXPROCS=1` или sleep перед сигналом больше окна) — показать в журнале.
- 20 прогонов `-count=20 -run TestSignalStopsTheCommand` под `graft exec --heavy` на занятой машине — без падений.

## Что
- 31.10 ~19:58 приёмка бойца exec-cores (acc1, /home/aisee/.local/state/worker/1031-1925-exec-cores/acc1.txt:114) на main 5b725de + правки: `TestSignalStopsTheCommand/terminated/task` — «command reached its marker»; машина под нагрузкой (runnable 16–27 из 22).
- caller_gone_integration_test.go: подтест task даёт шагу 0.6 с (exec — 0.45 с) от «ready» до SIGTERM через цепочку тест → graft → NotifyContext → StopOnCancel → trap; под нагрузкой окно не выдерживается. Продукт не виноват (натурная проверка на pc и мутации — тикет exec-caller-gone).


## Журнал
- 2026-10-31 20:03 открыт — graft-master
