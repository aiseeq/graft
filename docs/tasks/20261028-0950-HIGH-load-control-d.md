# heavy overflow to PC (D) and thread sharing (C)

Статус: готово
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Вариант D контроля нагрузки: излишок тяжёлого сверх ёмкости ноута уходит на PC для всех Go-проектов, общий путь без scripts/remote.sh в проектах; на PC та же очередь и мера akit/pressure; сбой пути → задача громко на ноуте + письмо мастеру. Остаток C в graft: потоки делятся между идущими задачами (или строка, что A+B закрыли). Сначала git-admin список: что graft нужно на PC (бинари, конфиг, папки, доставка при выпуске). Задание git-admin 28.10, решение пользователя `user-word …2dc3ff44….jsonl За ними C вместе с D`, рамка decisions pc-load-control, load-measure; зонтик ~/git/docs/tasks/20261028-0945-HIGH-load-control-cd.md; продолжение docs/tasks/20261027-2325-HIGH-heavy-queue.md.

## Приёмка
- Живой замер: ноут занят → задача на PC; путь сломан → ноут и письмо мастеру. Коммит/тег, независимая проверка.

## Журнал
- 2026-10-28 09:50 открыт — graft-master
- 2026-10-28 10:19 note — graft-master: проект D (graft-master 10:25):
1) remote_heavy в пользовательском конфиге (~/.config/graft/config.yaml) — для всех проектов; проектный перекрывает. Новый ключ when: busy|always (у пользовательского по умолчанию busy, у проектного always — как сейчас).
2) busy: шаг уходит на хост, только если очередь ноута его сейчас не пускает (тот же admit, что печатает waiting) и up ответил; иначе — здесь, как сейчас.
3) Что переносится с пользовательским remote_heavy: тяжёлая задача, все шаги которой — go test/go vet без флагов, пишущих файлы (-o, -c, -coverprofile, -cpuprofile, -memprofile, -trace, -json в файл), плюс правила stayHere (testdb, report, .env, dir, подпапка, файл сообщения); graft exec --heavy — по тем же правилам для argv. remote: true в задаче — явный допуск, remote: false — здесь.
4) Перевозка дерева — не в graft: run из конфига (кандидат — worker pc из llmark, у него копия дерева, go.work use, bwrap, код 125 = сбой связи). На PC run запускает graft exec --heavy --local -- …: та же очередь и мера на PC.
5) Сломанный путь: up не ответил или run вернул broken_exit (по умолчанию 125) → строка graft: <task>: <host> path broken: <причина>, running here и повтор здесь через очередь ноута; событие в журнал (~/.local/state/graft/remote-broken.jsonl: время, проект, задача, хост, причина, pid, cwd) — его читает aguard и пишет мастеру.
6) Доставка на PC: пользовательский install.hosts: [pc] — шаг install после атомарной установки здесь копирует тот же бинарь на хост по тому же пути (scp во временный + mv, mkdir -p); неудача — громкая строка, локальная установка остаётся.
7) Остаток C в graft: не нужен — admit по моментальному числу пускает задачу, только пока runnable < wait_above, и GOMAXPROCS = threads; после A+B наложений стартов 0 из 943, макс. load на старте 55 → 16.
- 2026-10-28 10:20 note — graft-master: выбор по модулям (возражение git-admin 10:28, верно): отсутствие модуля на PC — сбой пути, не провал теста. Вариант (а): перевозка до запуска проверяет, что модули go.sum дерева есть в кэше PC (или довозит их); нет — код 125 → graft запускает задачу на ноуте, строка и событие. Проекты с приватными модулями этим сами останутся на ноуте. Согласую с llmark-master (вопрос отправлен 10:26).
- 2026-10-28 10:43 note — graft-master: aguard-master 10:45: формат remote-broken.jsonl годится + поля claude_pid (CLAUDE_PID окружения graft, если есть) и worker (id задания worker из его переменной окружения); pid/ppid/cwd оставить запасным путём. Наблюдатель aguard — после коммита graft, письмо не чаще раз в 30 мин на (проект, хост).
- 2026-10-28 10:58 в работе — graft-master
- 2026-10-28 10:58 note — graft-master: боец Luna 1028-1058-d-overflow (класс фича-инструмента), дерево ~/.cache/worktrees/graft/d-overflow; мера — remote_overflow_integration_test.go мастера (красные 10:58 по нужной причине: user config не знает remote_heavy/install; TestProjectRemoteHeavyWins зелёный — охрана), sha в приёмке; бриф scratchpad d-overflow/brief.md. WORKER_JOB есть в llmark c962558. Путь журнала — user cache dir/graft/remote-broken.jsonl (рядом со stats.jsonl), не ~/.local/state — сообщить aguard-master.
- 2026-10-28 11:34 note — graft-master: боец 1028-1058-d-overflow: заходы 1–4 упирались в go fix -diff гейта на моём файле-спецификации (strings.Split → SplitSeq в readBroken); боец честно не трогал файл. Исправил файл сам, sha256 теперь 974e0bb9…, приёмка в job.json обновлена. Пять тестов у бойца проходили.
- 2026-10-28 11:48 note — graft-master: влито 260ef7e (боец + правки ревью: when: busy работает и у проектного; повтор по broken_exit только у пользовательского хоста, как в README; hosts читаются один раз). Дальше: релиз, install, конфиг pc с worker pc --tree (llmark f762116 поставлен), живой замер.
- 2026-10-28 12:05 note — graft-master: v2.47.0 выпущен и установлен (all зелёный); независимая проверка суб-агентом идёт.
- 2026-10-28 12:05 note — graft-master: llmark-master 12:0x: worker pc --tree пока НЕ включать в remote_heavy — блокер довоза модулей (боец 1028-1158-worker-pc-modules); попросил код 124 для таймаута на PC. Ждём сигнала.
- 2026-10-28 12:29 note — graft-master: git-admin добавил install.hosts [pc]; v2.47.1 скопирована на pc через install. remote_heavy — ждёт сигнала llmark о worker pc --tree
- 2026-10-28 12:36 note — graft-master: llmark 12:4x: worker pc --tree не включать (блокер у них: --timeout от начала вызова, ложный 124 при занятом PC; чинят в worker-pc-queue). На стороне graft взял: (1) remoteCmd без Cancel → SIGKILL, нужно SIGTERM+WaitDelay 30s; (2) задача с dir:/подпапка не уходит на хост (stayHere) → Dir=папка задачи, {root} в remote_heavy.run. Плюс glint: firstLine дубль tasks/tools, goCheckNoOutput cyclo 25, config-value-fallback userconfig When/BrokenExit.
- 2026-10-28 12:46 note — graft-master: Сделано в дереве (не закоммичено): {root} в remote_heavy.run → Dir = папка шага, без {root} задача с dir остаётся (причина в строке); SIGTERM при отмене + WaitDelay 30s (proc.StopOnCancel; Windows — kill); glint: Normalize(defaultWhen)+DefaultBrokenExit, firstLine-дубль снят, outputFlags таблицей. Тесты TestUserRemoteHeavyRunsATaskInItsDirectoryWithRoot, ...StopsACancelledStepWithSIGTERM (красный без StopOnCancel). README.
- 2026-10-28 12:58 note — graft-master: v2.48.0 (5c47481 в ней): {root}, SIGTERM+30s, установлена ноут+pc; независимая проверка — суб-агент 13:2x
- 2026-10-28 13:09 note — graft-master: Проверка v2.48.0: {root}/SIGTERM PASS; слабое место: kill через 30 с бил только run, дети (ssh) оставались. Исправлено: proc.StopOnCancel(cmd, grace, warn) — после grace kill потомков (/proc children, через pidfd-safe Signal(0)) и run; proc.Children перенесён из stats; юнит-тест TestStopOnCancelKillsTheTreeAfterGrace (красный без killTree).
- 2026-10-28 14:43 note — graft-master: 14:45 v2.48.4 (GOROOT на хосте, cache.files остаётся на ноуте) установлена; живой гейт glint с when: always — зелёный, 27 с, test-short остался на ноуте по cache.files; graft exec go test ./internal/gitx → on pc, 8 с; remote_heavy (when: busy) возвращён в ~/.config/graft/config.yaml
- 2026-10-28 15:01 note — graft-master: 15:1x независимая проверка (суб-агент): 338dff6, a2aada6 trimRun, cache.files — мутации краснеют, подтверждено. Дыры: (1) BusyNow считает очередь занятой и для процесса, который в неё не встал бы (GRAFT_ADMITTED, приоритет, вложенная heavy) → лишний перелив; (2) путь graft exec в trimRun без теста; (3) integration_test drop без GRAFT_HEAVY — тесты под graft exec --heavy / на хосте висят. Чиню в тикете remote-args вместе с {args}.
- 2026-10-28 15:54 готово — verify-subagent: живые замеры: ноут занят → pc 7,5 с; PC занят → ждал 27 с; обрыв → ноут + remote-broken.jsonl, aguard наблюдатель efd5508 прислал письмо 14:58 (aguard-master); гейт glint и test на PC зелёные; суб-агенты: 338dff6/a2aada6 подтверждены мутациями, дыры (admitted/вложенная/приоритет, exec-путь trimRun, GRAFT_HEAVY в тестах, {args}) закрыты в c3b67a0/15af99c; v2.49.1 на ноуте и pc; remote_heavy when: busy включён в пользовательском конфиге
