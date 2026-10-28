# graft exec --heavy --settle D

Статус: открыт

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- `graft exec --heavy --settle D -- …`: на этот вызов очередь занята D после впуска или до конца команды, что раньше (вместо load.settle). Просьба llmark-master 28.10 для допуска бойцов worker (тикет llmark docs/tasks/20261028-0945-HIGH-worker-load-gate.md): сейчас N бойцов проходят допуск каждые 10 с, нагрузка codex приходит позже.

## Приёмка
- Тест: два `exec --heavy --settle` подряд — второй ждёт ~D, пока первый ещё работает; первый кончился раньше — второй стартует сразу; `--settle` без `--heavy` и неверная длительность — usage; README и раздел help exec.

## Журнал
- 2026-10-28 11:50 открыт — graft-master
- 2026-10-28 11:54 note — graft-master: сделано в дереве main: main.go (--settle), internal/app/exec.go (ExecOptions, подмена Settle у r.Load), help.go, README, exec_settle_integration_test.go зелёный (без флага красный — usage).
