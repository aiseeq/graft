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
- 2026-10-31 16:18 note — graft-master: 16:2x замер на втором хосте: timeout 5 ssh <хост> graft exec --heavy --local -- sleep 41 → sshd-session ушёл, graft ppid 1, sleep 41 жив; fd0/fd1 — pipe. Допуск за секунды: живёт и запущенная задача. Вопрос 1 закрыт тестом TestGoTestThroughATerminal (\r\n и строки graft — те же runs). Ответ sc2go-master отправлен; план: SIGTERM/SIGHUP (не игнорированный) → отмена ctx; POLLERR/POLLHUP stdout/stderr (pipe/socket) → отмена; stdin и pdeathsig — нет (echo|graft, nohup). Дальше противник.
- 2026-10-31 16:18 в работе — graft-master
- 2026-10-31 16:25 note — graft-master: 16:25 противник (Terra) → приёмка в тикете; проверка брифа (Terra): одно расхождение — установка и замер на хосте у мастера, в бриф дописано. Боец Luna 1031-1625-exec-caller-gone (фича-инструмента), ветка exec-caller-gone; бриф и accept.sh — scratchpad мастера caller-gone/.

## Приёмка (противник замысла 16:3x, Terra; ответ — scratchpad caller-gone/answer.md мастера)
Принято:
- №1 один канал закрыт: сторож снимает, только когда И stdout, И stderr — трубы/сокеты и оба оборваны (один и тот же fd — один). Тест TestCallerGoneOneChannelKeepsRunning.
- №2 `| head` при stderr в файл или tty: сторож не ставится. Тест TestCallerGoneHeadWithStderrFile.
- №3 вложенный graft: внешний разбор читает до EOF; правило №1 закрывает. Отдельного теста нет.
- №4 deploy: `graft deploy` и graft с priority в env — сигналы как сейчас (только Ctrl-C), без сторожа. Тест TestDeployKeepsTodaysSignals.
- №7 гонка допуска и отмены: после допуска проверка ctx до запуска. Тест TestCancelAtAdmissionDoesNotStart.
- №8 потомки, игнорирующие SIGTERM: локальные шаги — StopOnCancel (SIGTERM, через срок killTree), а не SIGKILL одному лидеру. Тест TestLocalStepTreeKilledAfterGrace.
Отклонено:
- №5 SIGHUP фоновой задаче: сейчас SIGHUP и так убивает graft, оставляя потомка; nohup (SIGHUP проигнорирован при старте) не перехватываем — хуже не становится.
- №6 читающие концы труб у другого процесса: концы чтения держит только sshd-сеанс; гипотетично, признака нет.
Ещё: запись в оборванный stderr/stdout не должна убить graft SIGPIPE до снятия потомка (Go: запись в fd 1/2 без Notify на SIGPIPE завершает процесс) — signal.Notify, не Ignore (Ignore наследуется потомками).
После установки — повтор замера на втором хосте: `timeout 5 ssh <хост> 'graft exec --heavy --local -- sleep 41'` → через 5 с ни graft, ни sleep.
