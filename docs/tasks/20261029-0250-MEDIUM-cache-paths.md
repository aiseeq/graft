# cache.paths: ключ кеша задачи по части дерева

Статус: готово
Исполнитель: luna

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Запрос мастера потребителя (разбор тестов, 2026-10-29): shell-проверки гейта (shellcheck, bash-сьюты) идут на каждом коммите, хотя .sh меняются в 10 из 117 коммитов; это около половины гейта (11 с из 24.6 с медианы) и пик памяти 570 МБ.
- `cache: {paths: [glob…]}` — дерево в ключе задачи сужено до файлов под путями (глобы как cache_exclude; cache_exclude и файлы версии по-прежнему вне ключа). Остальной ключ прежний: определение, env, files, commands, аргументы, версия graft.
- Пути задачи = объединение её paths и paths всех deps и inner tasks; dep или inner task без paths (без cache или `cache: tree`) — ошибка конфига `tasks.<имя>.cache`: его вход — всё дерево. Пустой список paths — ошибка.
- README: раздел cache — paths, предупреждение: файл, который проверка читает вне путей (source, include), автор обязан включить в paths.

## Приёмка
- Приёмка мастера: accept_cache_paths_test.go (scratchpad accept/cachepaths) красный на main, зелёный после; `go run . all`; потребитель после установки — пропуск shell-задач на коммите без .sh.

## Журнал
- 2026-10-29 02:50 открыт — graft-master
- 2026-10-29 02:50 в работе — graft-master
- 2026-10-29 02:50 note — graft-master: боец Luna (id выше в worker status), worktree ~/.cache/worktrees/graft/cache-paths, приёмка accept/cachepaths.sh (красная на main)
- 2026-10-29 02:50 note — graft-master: потребитель: у обеих shell-задач нет deps и inner tasks; поставит paths: ['scripts/**', '.shellcheckrc'] после выхода версии
- 2026-10-29 03:30 note — luna: Добавлены cache.paths, проверка globs и deps/inner tasks; обновлён README. Приёмка accept/cachepaths.sh и graft gate прошли.
- 2026-10-29 03:51 готово — graft-master: мастер: ревью диффа; приёмка мастера (cache_paths_integration_test.go: пропуск при правке вне путей, запуск при правке под своими путями, путями dep и inner task, новом и удалённом файле, литеральном пути; ошибки конфига для dep без paths, cache: tree, пустого списка, плохого глоба) красная на main, зелёная после; прежние TestTaskCache* зелёные; обход deps+inner сведён в Task.Refs
