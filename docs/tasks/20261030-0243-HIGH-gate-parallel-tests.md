# Гейт graft выше 60 с: тесты корневого пакета идут строго подряд

Статус: ждёт
Ждёт: 2026-10-31 12:00
Исполнитель: graft-master

## Вход
- Задание Сна (обход tests 30.10 02:40 CET, итог — git-admin): гейт p50 64 с (29.10 12:00–30.10 03:00) при пределе 60 с. Шаг `go test -json -p 4 -count=1 -short ./...` — 61.7 с; корневой пакет — 265 тестов, сумма их времён почти равна wall: t.Parallel() нет ни в одном корневом *_test.go.
- Ожидание Сна: корневой пакет 70 → ~20 с при -parallel 4.

## Что сделать
- Выяснить, что мешает t.Parallel: общее состояние фикстур (HOME, XDG_*, кеш graft, load.lock, порты, testdb, os.Setenv в тестах, общий бинарь).
- Сделать фикстуры независимыми и включить t.Parallel там, где тесты не трогают общее; оставшиеся подряд — с причиной.
- Решить про -count=1 для остальных пакетов в гейте.

## Приёмка
- Время шага гейта до/после (graft stats --task gate), `go test -race` корневого пакета зелёный несколько раз подряд (-count=3), `go run . all`.

## Журнал
- 2026-10-30 02:43 открыт — graft-master
- 2026-10-30 02:43 в работе — graft-master
- 2026-10-30 03:48 note — graft-master: t.Parallel отклонён по правилу 5 test-optimization (параллельность не ускорение): test-gate wall p50 59.5 с, CPU 1m3s — CPU-bound; план — профиль CPU корневого пакета; сообщено git-admin
- 2026-10-30 05:25 note — graft-master: Снятие -count=1 в test-gate (условие git-admin: внешние входы детей). Пробы до правки: GIT_AUTHOR_DATE=bogus и правка testdata/gatehelper — корневой пакет '(cached) ok' при настоящем FAIL (-count=1: fatal: invalid date format). Правка: internal/testinputs (Env — все унаследованные переменные кроме per-run graft и список newlySet; Files — исходники gatehelper), тест TestChildInputsKeyTheCache в корне и 13 пакетах, чьи тесты запускают процессы. После: оба входа — промах кеша и FAIL (корень 30 с / 63 с, internal/deploy FAIL); два test-gate подряд 66.8 с → 5.9 с (26/26 cached). Вне ключа остаются бинари вне модуля (git, sh, docker) и новые переменные вне newlySet — их ловит test-race (-count=1) в all перед релизом.
- 2026-10-30 05:39 note — graft-master: Замер кеша гейта на коммите только с тикетом, заход 1.
- 2026-10-30 05:40 note — graft-master: Замер кеша гейта на коммите только с тикетом, заход 2.
- 2026-10-30 05:43 note — graft-master: Замер кеша гейта после исключения GRAFT_VERSION, заход 1.
- 2026-10-30 05:44 note — graft-master: Замер кеша гейта после исключения GRAFT_VERSION, заход 2.
- 2026-10-30 05:45 note — graft-master: Итог шага: -count=1 снят (18371b3, 8360bf4, 38054d5). Гейт docs-коммита 64–71 с → 4.3 с wall, 66–71 → 3 CPU-с (stats.jsonl 05:44:20); код-коммиты без изменений 65–71 с. Числа отправлены git-admin 05:46. Дальше: гейт код-коммитов > 60 с.
- 2026-10-30 06:50 note — graft-master: Профиль корневого пакета (perf -e cpu-clock:u, 30.10 06:45): user 28 с, sys 43 с; доли user — git 26%, compile 19%, link 14%, graft 13%, go 13%, vet 5%. Запусков ~18.5k, из них git от graft 10.6k, повторы тех же аргументов в одном процессе graft 2430 (rev-parse --verify HEAD^{commit} 566, write-tree 333, show-toplevel 213, symbolic-ref 189, status 160, rev-parse HEAD 160, add -A 138). Мера: /home/aisee/.cache/worktrees/graft-measure/git-calls.sh (гейт + число git-вызовов graft в корневом пакете), база 10822 на 1a4642e, порог 9000. Дальше — боец Luna (класс оптимизация, luna-мера).
- 2026-10-30 06:51 note — graft-master: Боец Luna 1030-0651-perf-git-call-dups запущен: бриф /home/aisee/.cache/worktrees/graft-measure/brief-git-call-dups.md, дерево /home/aisee/.cache/worktrees/graft/perf-git-call-dups (ветка perf/git-call-dups), приёмка git-calls.sh 9000. После приёмки — своё ревью diff (безопасность повторного использования ответа git) и независимая проверка.
- 2026-10-30 08:22 note — graft-master: 08:23 CET: боец 90 мин, заход 1: мера 10822 → 9694 (порог 9000), профилирует остаток; не застрял — жду до 09:05.
- 2026-10-30 09:03 note — graft-master: 09:05 CET: боец снят (132 мин, мера 9192 из 10822, порог 9000 не взят; гонялся за установкой graft — тесты её не используют, install graft сам отказал из linked worktree, ~/bin/graft цел 06:37). worker continue: убрать кэши без выигрыша, тесты сброса, gate, коммит, отчёт. Дальше — свой замер CPU/wall test-gate до/после и ревью.
- 2026-10-30 09:18 note — graft-master: 09:25 CET: правка бойца отклонена — мера вызовов 10822→9192, но CPU корневого пакета (go test -short -count=1 ., по 2 прогона попеременно) 47.3→46.8 с при шуме ±4 с; кэш ответов git с белым списком мутирующих подкоманд (не ловит -c … commit, Command(), внешние шаги) — риск устаревших ответов без выигрыша. Архив archive/git-call-dups (4cf0e6c), ветка и дерево удалены, worker done отклонено. Мера мастера (число вызовов) была не той. Находка: корневой пакет wall 64 с при CPU 47 с — ~17 с ожидания; гейт за 4 ч: median 1m12s, test-gate 1m5s / CPU 57.6 с (load<16).
- 2026-10-30 09:27 note — graft-master: Простой по тестам (prof/idle.sh, каждый тест отдельно, простой = wall − CPU): 12 тестов ≈ 20 с, все — нарочные ожидания (calmAfter, settle, выборки памяти/диска по 200 мс, WaitDelay 2 с у FailReasons). Правка: calmAfter возвращает stop — ExecOfGraft не ждёт таймер 3 с после graft (3.24 → 1.10 с); три варианта KeepsHere — один гейт с тремя задачами (5.0 → 1.7 с). Мутация (без remoteVouched в prepareGateStep) роняет KeepsHere на local-heavy и local-plain.
- 2026-10-30 09:29 note — graft-master: Перенос в test-race (all), правило 4 test-optimization («длинные сценарии — стадия перед release»): 6 тестов с нарочными ожиданиями в стабильном коде под skipWaitInShort — FailReasonsDoNotWaitForABackgroundChild 2.2 с, StatsMarkTasksOverTheLimits 1.9, StepDiskIO 1.7, StepMemoryOfTheProcessTree 1.6, ExecSettleHoldsTheQueueForThisCommand 1.4, StatsRecordTheMachineLoad 1.2 (≈10 с wall). Тесты remote/split (активная зона) остаются в гейте. Строка CLAUDE.md:8 — пункт Сну в project-queue.md.
- 2026-10-30 09:31 ждёт — graft-master — 2026-10-31 06:10
- 2026-10-31 02:29 note — graft-master: Сон 31.10 02:50 (через dream-master, итог git-admin): корневой пакет 265→321 тест, test-gate wall 75 с; 36 новых тестов заморозки (Freeze/Frozen/Advisory/Round9/Thaw) по 1.3–2.5 с, 24–55 с на прогон. Вопрос: что в них стоит 1–2 с, нужны ли в гейте целиком.
- 2026-10-31 02:29 в работе — graft-master
- 2026-10-31 03:16 note — graft-master: Тесты заморозки 31.10: 1–2 с на тест были ожиданием, не работой. Фикстуры, писавшие свой user config (deploy, install, modules, stats, leaks, tools и др., 12 мест), теряли тестовый load: каждый гейт внутри теста ждал swap_window по умолчанию — 1 с (strace: 1.0 с между ls-files */go.mod и стартом гейта, два чтения vmstat с интервалом 1 с; userconfig.DefaultLoad). Правка 3a14f76: testLoadConfig и writeUserConfig, сторож TestUserConfigsCarryTheTestLoad (красный на возвращённой ошибке). A/B тестовых бинарей поочерёдно, -test.v, круг 2 (load 7.6 / 4.1): 39 тестов заморозки 25.3 → 15.8 с, остальные 50.2 → 51.4 (шум), весь пакет 75.5 → 67.2 с; CPU без изменений (62.7 / 65.9 с) — снято ожидание, а не добавлены ядра. Гейт коммита 3a14f76 шёл 3m57s при load 21–39 (четыре бойца) — не в счёт. Дальше: p50 test-gate на main при load<16 после 3a14f76.
- 2026-10-31 03:16 ждёт — graft-master — 2026-10-31 12:00
