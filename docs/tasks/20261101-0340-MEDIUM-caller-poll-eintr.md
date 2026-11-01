# сторож вызвавшего навсегда перестаёт следить после одного EINTR в unix.Poll

Статус: в работе
Исполнитель: graft-master

## Вход
- Независимая проверка glint-debt-reserve 01.11: в логе go run . all строка `graft: caller watcher poll: fd 1: interrupted system call`. internal/proc/caller_unix.go pollCallerGone не повторяет unix.Poll при EINTR; WatchCaller на ошибке пишет строку и выходит — снятие при уходе вызвавшего (59eb417) больше не работает для этой команды.

## Что сделать
- EINTR в unix.Poll — повтор того же опроса (не ошибка); прочие ошибки — как сейчас.

## Приёмка
- Тест: подменённый poll возвращает EINTR один раз, затем POLLHUP на всех — сторож отменяет контекст с errorsCallerGone; красный на старом коде.
- go run . all зелёный.

## Журнал
- 2026-11-01 03:40 открыт — graft-master
- 2026-11-01 03:55 в работе — graft-master
