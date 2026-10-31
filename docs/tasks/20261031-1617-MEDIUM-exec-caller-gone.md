# graft exec: не стартовать и снимать задачу, когда вызвавший ушёл (обрыв ssh без tty, SIGTERM, SIGHUP)

Статус: готово
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
- 2026-10-31 18:10 note — graft-master: 18:3x Luna: принято (93 мин, 0.029% недели OpenAI), мутации: без SIGTERM → красный TestSignalStopsTheCommand/terminated; по одному каналу → красный OneChannelKeepsRunning. Ревью мастера: только exec (нужно все команды кроме deploy/priority), localStopGrace 300 мс → 10 с переменной, env-интервал с молчаливым фолбеком, recover в сторожке. worker continue.
- 2026-10-31 18:48 note — graft-master: 18:4x Luna: правки по 5 пунктам ревью, идёт приёмка; моё ревью diff: наблюдатель только при обоих FIFO/сокетах, StopOnCancel 10 с, ctx до старта, deploy/priority исключены; akit в go.mod в патч не беру.
- 2026-10-31 19:22 note — graft-master: 19:21 влито 59eb417 (дерево Luna 3 раунда: область — все команды кроме deploy/priority, grace 10 с, ошибка env интервала, без recover; гонка TestWaitNamesWhoHeldTheQueue перенесена на версию main руками), all зелёный, v2.69.0 на ноуте и pc. Замер на pc 19:21:49: timeout 5 ssh pc 'graft exec --heavy --local -- sleep 41' → exit 124, через ~5 с pgrep 'sleep 41' пусто, своих graft exec нет. Дальше — независимая проверка, sc2go-master.
- 2026-10-31 19:27 note — graft-master: 19:3x независимая проверка (суб-агент Opus, бриф scratchpad caller-gone/verify-brief.md): 5 из 6 сценариев да, натурный замер на pc да (с проверкой посередине: graft и sleep живы под timeout, через 6 с после exit 124 — нет); №7 нет — тест проверял отмену в ожидании, мутация «убрать ctx.Err() в execCommand» зелёная. Разбор: гонку закрывает exec.Cmd.Start (исходник Go 1.26 проверен), проверка убрана как дубль, тест переименован в TestSigtermWhileWaitingDoesNotStart, приёмка №7 переписана.
- 2026-10-31 19:34 готово — verifier-opus (session graft-master): суб-агент Opus 31.10 19:2x–19:4x (бриф scratchpad graft-master caller-gone/verify-brief.md): сценарии №1,2,4,8, SIGPIPE — тесты PASS, мутация Notify(SIGPIPE) роняет TestCallerGoneStopsTheCommand; натурно на pc 19:23 — под timeout graft и sleep 41 живы, через 6 с после exit 124 нет; №7 после 5b725de — exec.Cmd.Start (Go 1.26 exec.go:702–706) не стартует при отменённом ctx, тесты PASS (лог caller-gone/verify-test2.log); glint: 11 findings, debt glint-debt-reserve

## Приёмка (противник замысла 16:3x, Terra; ответ — scratchpad caller-gone/answer.md мастера)
Принято:
- №1 один канал закрыт: сторож снимает, только когда И stdout, И stderr — трубы/сокеты и оба оборваны (один и тот же fd — один). Тест TestCallerGoneOneChannelKeepsRunning.
- №2 `| head` при stderr в файл или tty: сторож не ставится. Тест TestCallerGoneHeadWithStderrFile.
- №3 вложенный graft: внешний разбор читает до EOF; правило №1 закрывает. Отдельного теста нет.
- №4 deploy: `graft deploy` и graft с priority в env — сигналы как сейчас (только Ctrl-C), без сторожа. Тест TestDeployKeepsTodaysSignals.
- №7 гонка допуска и отмены: закрыта самим exec.Cmd.Start — при отменённом ctx процесс не стартует (Go 1.26 os/exec/exec.go:702–706), после старта снимает StopOnCancel; своя проверка ctx в execCommand была дублем и убрана. Отмена во время ожидания — TestSigtermWhileWaitingDoesNotStart (SIGTERM) и TestCallerGoneWaitingDoesNotStart (обрыв труб).
- №8 потомки, игнорирующие SIGTERM: локальные шаги — StopOnCancel (SIGTERM, через срок killTree), а не SIGKILL одному лидеру. Тест TestLocalStepTreeKilledAfterGrace.
Отклонено:
- №5 SIGHUP фоновой задаче: сейчас SIGHUP и так убивает graft, оставляя потомка; nohup (SIGHUP проигнорирован при старте) не перехватываем — хуже не становится.
- №6 читающие концы труб у другого процесса: концы чтения держит только sshd-сеанс; гипотетично, признака нет.
Ещё: запись в оборванный stderr/stdout не должна убить graft SIGPIPE до снятия потомка (Go: запись в fd 1/2 без Notify на SIGPIPE завершает процесс) — signal.Notify, не Ignore (Ignore наследуется потомками).
После установки — повтор замера на втором хосте: `timeout 5 ssh <хост> 'graft exec --heavy --local -- sleep 41'` → через 5 с ни graft, ни sleep.
