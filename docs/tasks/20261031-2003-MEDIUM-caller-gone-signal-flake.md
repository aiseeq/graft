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
- 2026-10-31 20:58 note — graft-master: 21:0x боец Luna (правка-инструмента), дерево /home/aisee/.cache/worktrees/graft/caller-gone-signal-flake; бриф и мера — scratchpad graft-master flake/{brief.md,accept.sh} (20 прогонов, правки только в тестах).
- 2026-10-31 21:14 note — graft-master: 21:1x Luna приняла: ожидание смерти pid ребёнка (kill 0 → ESRCH, потолок 5 с) вместо маркера в 4 тестах; красный старого при задержке 700 мс перед сигналом, новый зелёный; 20 прогонов зелёные. Ревью: в TestLocalStepTreeKilledAfterGrace TERM игнорируется наследованием, так что смерть sleep доказывает killTree. Влито ff 1f7a1e3. Дальше — независимая проверка.
