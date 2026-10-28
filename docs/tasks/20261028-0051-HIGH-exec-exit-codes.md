# graft exec tells its own failure from the command's by exit status

Статус: открыт

## Вход
- Проблема: `graft exec` exits 1 on its own failure (config, queue, lock, command not found) as on the command's; a caller (a benchmark judge) counts graft's failure as the command failing. Request: a peer session, 28.10 ~00:45.
- Метод: as env/timeout: 125 graft itself (usage too), 126 the command cannot be started, 127 not found; the command's own status unchanged, a signal 128+N; only the command's result counts as its status (internal/tasks Runner.Exec marks it).
- Инструмент: internal/app/exec.go, internal/tasks Runner.Exec, main.go execCmd, README.
- Цена: an hour.
- Если не делать: false failures in the benchmark.

## Что сделать
1. Mark the command's result in Runner.Exec; map in App.Exec; usage → 125.
2. README `graft exec` row: the codes; remote_heavy: ssh's own failure is 255.
3. Tests: missing command 127, non-executable 126, bad config / usage 125, command exit 3 → 3.

## Приёмка
- `go run . all`; install; reply to the requester with the version.

## Журнал
- 2026-10-28 00:51 открыт — graft-master
- 2026-10-28 00:58 done in code: tasks.CommandError marks the command's result; App.Exec maps 125/126/127, 128+signal; usage 125; README; TestExecExitStatus
- 2026-10-28 01:02 note — graft-master: independent review (fresh agent) of c4b0f32: -C and early main errors exit 1/2, an unstartable remote_heavy run maps to 126/127, Ctrl-C reads as the command's 137; fixed: execCall routes every exec failure but the command's to 125, a run not started is graft's, an interrupt is 125; tests for -C and the run (the interrupt untested)
