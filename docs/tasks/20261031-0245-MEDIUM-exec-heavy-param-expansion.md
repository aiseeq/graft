# graft exec --heavy отказывает команде с раскрытием параметра ${w%|*}

Статус: на проверке
Исполнитель: graft-master

## Вход
- Задание Сна, обход tools, 31.10 (через dream-master, итог — git-admin). Проба v2.62.0: `graft exec --heavy -- bash -c 'w="a|b"; echo ${w%|*}'` → `graft: invalid variable name "w%|*"`, rc=125; без --heavy и с --heavy --local печатает `a`.
- Случаи за 7 дней: 2 (`${item%%:*}`, `${w%|*}`), агенты переписывали команду.
- По чтению Сна: internal/tasks/remote.go dotenvRef → dotenv.Refs на argv; internal/dotenv/lookup.go:125 ошибка на `${…}` с не-идентификатором роняет весь exec.

## Что сделать
- Не-идентификатор в `${…}` argv — не ссылка на ключ .env, а не ошибка exec (проверить, кто ещё зовёт Refs и где ошибка нужна).

## Приёмка
- Тест: exec --heavy с `${w%|*}` запускает команду; ссылка `${KEY}` на ключ .env по-прежнему распознаётся.

## Журнал
- 2026-10-31 02:45 открыт — graft-master
- 2026-10-31 03:16 note — graft-master: Сделано в 3a14f76: dotenv.ShellRefs (нестрогий разбор: ${…} не-ключ — раскрытие оболочки), его берут dotenvRef (remote.go) и проверка ключей задачи (tasks.go) для argv; env задач и deploy/envs остаются строгими. Тесты: TestShellRefs, шаг в TestExecHeavyRunsOnRemoteHost (без фикса: invalid variable name "w%|*", с фиксом — команда на хосте, печатает a).
- 2026-10-31 03:16 на проверке — graft-master — 3a14f76
