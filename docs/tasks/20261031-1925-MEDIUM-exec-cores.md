# graft exec --heavy --cores N: явный резерв ядер

Статус: открыт

## Что сделать

## Приёмка
- `--cores N`: N — число > 0 и ≤ числу потоков машины, иначе ошибка использования (код 125) с текстом; без `--heavy` — ошибка использования, как `--settle`.
- Тест: exec --heavy --cores 1.5 пишет живой резерв с cores 1.5 (не историю и не 4); stats-запись reserve_cores 1.5.
- Тест: без --cores — по-прежнему история (существующие тесты резерва зелёные).
- `--cores` вместе с `--settle` — оба действуют.
- README: строка exec в таблице команд и абзац про --settle/--cores; `graft help`/usage — с --cores.
- `graft gate` зелёный, `go run . all` перед релизом.

## Что
- Запрос соседней сессии 31.10 19:4x (её тикет worker-graft-reserve): `graft exec --heavy --cores N` — резерв N ядер вместо оценки по истории load.cores.json.
- Зачем: допуск бойца — команда `sh -c 'echo …; exec sleep 20'`, нагрузка (агент) стартует вне её дерева; история профиля `<проект>:exec:sh` даёт ≈0.5 ядра, пачка бойцов на свободной машине резервом не сдерживается; сейчас у них `--settle 20s` (сериализация, ради снятия которой сделан резерв).
- N дробное; потребитель ставит 1. Снятие — как у любого резерва: CPU дерева ≥ N/2 за 2 с, конец команды, load.settle.
- Новым механизмом не считаю: параметр существующего резерва (противник резерва — тикет queue-adversary), противник не нужен.


## Журнал
- 2026-10-31 19:25 открыт — graft-master
- 2026-10-31 19:25 note — graft-master: 19:5x боец Luna 1031-1925-exec-cores, дерево /home/aisee/.cache/worktrees/graft/exec-cores; бриф и мера — scratchpad graft-master exec-cores/{brief.md,accept.sh}. Брифом проверен worker run.
- 2026-10-31 20:12 note — graft-master: 20:1x Luna приняла (2 захода: 1-я приёмка упала на чужом флейке TestSignalStopsTheCommand — тикет caller-gone-signal-flake, боец его не трогал). Моё ревью: разбор и границы N верны, тест ловит подмену (история 4 → резерв 1.5), удалённый путь не нужен (sh не go test). Правки сам: обёртка WaitWithSettleCores слита в WaitWithSettle с параметром cores (без полу-алиаса), фраза README перенесена из середины абзаца --settle. Ветка 13fa924, на main — патчем (main ушёл на 5b725de).
- 2026-10-31 20:35 note — graft-master: 20:35 v2.70.0 (all зелёный после dd8fdbd), install ноут+pc; живая проба --cores 1 → живой резерв {"cores":1,...,"task":"exec"}. llmark-master уведомлён. Идёт независимая проверка (суб-агент Opus).
- 2026-10-31 20:38 note — graft-master: 20:4x независимая проверка (суб-агент Opus): пункты 1–7 да (границы N пробами, мутации expectedReserveCores и admit ловятся TestExecCoresReservesGivenCores, верхняя граница — TestExecCoresUsage, живой резерв 1.5 с --settle 3s), п.8 нет — graft help exec без --cores (help.go:72). Исправлено: const execSyntax для usage и help, строка --cores в details; общий разрыв — тикет usage-help-drift.
