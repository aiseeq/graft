# graft exec: не стартовать и снимать задачу, когда вызвавший ушёл (обрыв ssh без tty, SIGTERM, SIGHUP)

Статус: в работе
Исполнитель: graft-master

## Что сделать

## Приёмка
- Вопрос 1: разбор `tests: go` (internal/testreport/gotest.go) переживает \r\n и строки graft между событиями — тест на смесь.
- Вопрос 2: замер на хосте до правки (жив ли sshd-сеанс, ppid graft, состояние stdin), противник замысла до кода, тест обрыва.

## Что
- Запрос соседней сессии (сценарий С8 допуска на втором хосте): `ssh host 'graft exec --heavy --local -- sleep 300'`, клиент ssh убит через 5 с — через 120 с graft и sleep на хосте живы. Задача, ждущая допуска, стартует без клиента.
- main.go:72 — NotifyContext только на os.Interrupt; SIGTERM и SIGHUP убивают graft по умолчанию, дочерний процесс остаётся.
- Обход у потребителя — `ssh -tt`: stderr сливается в stdout, строки с \r\n.


## Журнал
- 2026-10-31 16:17 открыт — graft-master
- 2026-10-31 16:18 note — graft-master: 16:2x замер PC: timeout 5 ssh pc graft exec --heavy --local -- sleep 41 → sshd-session ушёл, graft ppid 1, sleep 41 жив; fd0/fd1 — pipe. Допуск за секунды: живёт и запущенная задача. Вопрос 1 закрыт тестом TestGoTestThroughATerminal (\r\n и строки graft — те же runs). Ответ sc2go-master отправлен; план: SIGTERM/SIGHUP (не игнорированный) → отмена ctx; POLLERR/POLLHUP stdout/stderr (pipe/socket) → отмена; stdin и pdeathsig — нет (echo|graft, nohup). Дальше противник.
- 2026-10-31 16:18 в работе — graft-master
