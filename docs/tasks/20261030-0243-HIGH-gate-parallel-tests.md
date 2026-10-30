# Гейт graft выше 60 с: тесты корневого пакета идут строго подряд

Статус: в работе
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
