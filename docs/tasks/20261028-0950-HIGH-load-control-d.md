# heavy overflow to PC (D) and thread sharing (C)

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
