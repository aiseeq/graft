# сторож вызвавшего навсегда перестаёт следить после одного EINTR в unix.Poll

Статус: на проверке
Исполнитель: graft-master

## Вход
- Независимая проверка glint-debt-reserve 01.11: в логе go run . all строка `graft: caller watcher poll: fd 1: interrupted system call`. internal/proc/caller_unix.go pollCallerGone не повторяет unix.Poll при EINTR; WatchCaller на ошибке пишет строку и выходит — снятие при уходе вызвавшего (59eb417) больше не работает для этой команды.

## Что сделать
- EINTR в unix.Poll — не ошибка: ответа нет, тот же опрос — на следующем тике (немедленный повтор крутил бы CPU при сигналах подряд); прочие ошибки — как сейчас.

## Приёмка
- Тест: подменённый poll возвращает EINTR один раз, затем POLLHUP на всех — сторож отменяет контекст с errorsCallerGone; красный на старом коде.
- go run . all зелёный.

## Журнал
- 2026-11-01 03:40 открыт — graft-master
- 2026-11-01 03:55 в работе — graft-master
- 2026-11-01 03:59 note — graft-master: ce5358f на main: unixPoll — шов для теста, повтор при EINTR; тест TestCallerGonePollRetriesAnInterruptedPoll красный до правки (FAIL), после — пакет -race -count=3 зелёный. Дальше — независимая проверка вместе с другими перед релизом.
- 2026-11-01 03:59 на проверке — graft-master
- 2026-11-01 04:54 note — graft-master: проверка суб-агентом 04:47: ce5358f принят (мутация красная, all зелёный), замечание — повтор на EINTR без предела: при постоянном EINTR 15,5 млн вызовов за 500 мс. Правка: EINTR → (false, nil), опрос на следующем тике; тесты TestCallerGonePollLeavesAnInterruptedPollToTheNextTick (1 вызов на EINTR) и TestCallerWatcherSurvivesAnInterruptedPoll (WatchCaller отменяет с errorsCallerGone после 3 EINTR — пункт приёмки про отмену контекста); мутация без обработки EINTR — оба красные. sc2go-master 04:42 видел ту же строку на PC под v2.69.0 — уйдёт с релизом.
- 2026-11-01 05:30 note — graft-master: Вторая проверка (суб-агент ac597d262bf5bf326): принято — мутации p1 (без ветки EINTR) и p2 (немедленный повтор) красные, без мутаций зелёные, причина errorsCallerGone. Поле «Что сделать» приведено к сделанному (следующий тик).
