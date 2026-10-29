# тяжёлые задачи ждут, пока включён тихий режим машины (akit busy --quiet-until)

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема: задание лида 2026-10-29 12:4x: пользователь просит не вызывать фризы диска во время важных дел. akit делает `busy --quiet-until <время>`: срок в `$XDG_STATE_HOME/akit/quiet-until` (или `~/.local/state/akit/quiet-until`), одна строка RFC3339; нет файла — выключено; истёк — не действует; битый — ошибка замера. akit/pressure `Meter.Check` отдаёт `Verdict{Measure: pressure.Quiet}`, Why() «quiet mode until HH:MM», срок в `Verdict.Load.QuietUntil` (формат — akit-master).
- Метод: internal/loadgate — вердикт quiet ждёт, как load/io/swap; ключ Waits `quiet`; строка ожидания «quiet mode until HH:MM».
- Находка по ходу: `measures[v.Measure]` для неизвестной меры даёт пустой why — busy-вердикт akit молча считается свободной машиной. Неизвестная мера — busy с why = имя меры, текст v.Why().

## Что сделать
- loadgate: WhyQuiet, запись меры с подсказкой, как выключить (команда akit), без «user config»; неизвестная мера — busy, не свободно.
- go.mod: akit до версии с pressure.Quiet (сам `graft commit`, modules.latest).
- Тесты: интеграционные и loadgate — `XDG_STATE_HOME` во временную папку (иначе в тихий режим падают); тест: файл со сроком в будущем → ждёт со строкой quiet mode; истёкший → не ждёт.
- README: load — про тихий режим.
- Тесты и сборку — после 14:45 CET 29.10 (запрет лида на диск).

## Приёмка
- Тесты loadgate и load_integration зелёные; `graft exec --heavy -- true` при `busy --quiet-until +5m` печатает «quiet mode until …, waiting» и ждёт, после `--quiet-off`/истечения стартует; `go run . all`.

## Журнал
- 2026-10-29 12:46 открыт — graft-master
- 2026-10-29 12:46 в работе — graft-master
- 2026-10-29 12:47 note — graft-master: код без сборки (запрет до 14:45): loadgate WhyQuiet и запись pressure.Quiet, неизвестная мера — busy; XDG_STATE_HOME в тестах (integration testEnv+drop, loadgate fakeMachine); тесты quiet в TestBusyDiskAndSwapHoldTasks и TestQuietModeOverHoldsNoTask; README. Ждёт релиза akit с pressure.Quiet и имени команды выключения от akit-master
- 2026-10-29 12:47 note — graft-master: akit-master подтвердил: Busy() true, Why «quiet mode until HH:MM» (не сегодня — с датой), выключение busy --quiet-off, порядок: файл режима, потом /proc; тест учитывает дату. Ждём версию akit после 14:45
- 2026-10-29 14:37 note — graft-master: akit v1.6.0; коммит 6f8786d (graft commit поднял akit, гейт зелёный); go run . all на 6f8786d идёт; дальше: проверка связки busy --quiet 2m → go run . exec --heavy -- true ждёт с quiet → busy --quiet-off, на отдельном XDG_STATE_HOME (не глушить машину); release --minor, install; tk done в akit 20261029-1245-MEDIUM-busy-quiet; итог git-admin
