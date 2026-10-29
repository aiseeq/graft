# exec --heavy: откат на эту машину при broken_exit хоста

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Замер 2026-10-29 04:58: `graft exec --heavy -- go test …` ушёл на хост remote_heavy пользователя; его run (обёртка хоста) отказал с кодом 125 (хост в карантине) — graft вернул exit 125, команда не выполнилась нигде.
- README (remote_heavy): у хоста из user config выход run с кодом `broken_exit` — сломанный путь, graft пишет причину и запускает здесь через очередь heavy, строка в remote-broken.jsonl. Для задач так и есть (internal/tasks/tasks.go, runTask: remoteBrokenReason → steps снова), в `Runner.Exec` (там же, около строки 931) этого нет: noteBrokenRemote не зовётся, отката нет.
- Сделать: Exec при f.remote и выходе run == broken_exit у пользовательского хоста — та же строка «graft: exec: <host> path broken: <причина>, running here», журнал remote-broken.jsonl, повтор команды здесь через enterHeavy. Одна логика с задачами (общая функция), не копия.

## Приёмка
- accept_exec_broken_test.go (scratchpad accept/execbroken): красный на main, зелёный после; прежние TestBrokenRemotePath*, TestUserRemote*; `go run . all`.

## Журнал
- 2026-10-29 04:59 открыт — graft-master
- 2026-10-29 05:04 в работе — graft-master
- 2026-10-29 05:04 note — graft-master: боец Luna (worker status: exec-broken-remote), worktree ~/.cache/worktrees/graft/exec-broken-remote, приёмка accept/execbroken.sh (красная на main 05:04: exit 125, нет строки path broken)
- 2026-10-29 05:13 note — luna: luna: Общий откат broken_exit для задач и graft exec --heavy; Exec повторяет команду локально с локальными threads, README обновлён.
- 2026-10-29 05:13 note — graft-master: ревью мастера: откат — одна функция fallbackBrokenRemote для задачи и exec (frames — указатели, сброс remote виден closure exec); комментарий к ней, приёмка перенесена в remote_overflow_integration_test.go (TestExecBrokenRemotePathRunsHere); удалённые тесты зелёные
