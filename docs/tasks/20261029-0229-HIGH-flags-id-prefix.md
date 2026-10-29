# graft flags show/ack/mute принимают однозначный префикс id

Статус: готово
Исполнитель: luna

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Задание git-admin (лид, Сон, обход logs, ночь 2026-10-29).
- `graft flags show|ack|mute` сейчас принимают только полный UUID; короткий id из `graft flags status` даёт у одного адаптера `event … not found`, у другого HTTP 400 от среды — и ложный флаг client_reject у потребителя.
- Принимать однозначный префикс id (из того же списка, что `flags status`); неоднозначный или ненайденный — ошибка до запроса к среде, с перечнем кандидатов для неоднозначного.
- Итог — git-admin.

## Приёмка
- Тесты: префикс → полный id в запросе; неоднозначный/ненайденный → ошибка, запроса к среде нет (httptest и SQL-адаптер).
- `go run . all` зелёный; `go run . install`.

## Журнал
- 2026-10-29 02:29 открыт — graft-master
- 2026-10-29 02:41 в работе — graft-master
- 2026-10-29 02:44 note — graft-master: боец Luna 1029-0244-flags-id-prefix (класс фича-инструмента, escalate terra — порядок лида до 31.10 09:00), worktree ~/.cache/worktrees/graft/flags-id-prefix; приёмка — scratchpad accept/flags.sh: accept_prefix_test.go (красный на main) + graft gate
- 2026-10-29 03:43 note — graft-master: боец: общее разрешение id по открытым событиям для show/ack/mute; мастер: текст ошибки, README (только открытые события), приёмка → постоянный internal/flags/idprefix_test.go
- 2026-10-29 03:43 готово — graft-master: мастер: ревью диффа; приёмка мастера (internal/flags/idprefix_test.go: префикс → полный id в Get/Resolve и выводе, неоднозначный и ненайденный — ошибка без Get/Resolve и без правила, точный id побеждает более длинный) красная на main, зелёная после; гейт в коммите
