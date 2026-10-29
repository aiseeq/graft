# История публичных репо aiseeq/* без имён и данных рабочих проектов

Статус: на проверке
Ждёт: итоги chooks-master и glint-master через git-admin; затем независимая проверка свежими клонами
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Задание git-admin (лид, Сон шаг 3, ночь 2026-10-29). Решение пользователя: `~/.claude/decisions.md` public-repo-names (28.10 09:53 CET) — в публичных репо вычищать только имена и данные рабочих проектов (термины `~/.config/graft/private-terms` + `leaks.private_sources` из `~/.config/graft/config.yaml`).
- 1) Поиск: каждый публичный репо aiseeq/* — свежий mirror-клон, `graft leaks` по всей истории всех веток и тегов; отдельно сообщения merge-коммитов и аннотированных тегов (graft leaks пропускает merge).
- 2) Правка истории: репо с находками — git filter-repo (замена текста в сообщениях и блобах), force-push всех веток и тегов; в graft — известная находка в сообщении коммита (из дерева убрано 7b5ec19).
- 3) Местные клоны и worktree переписанных репо привести к новой истории (у чужих репо — их мастерам).
- Итог — git-admin через SendMessage.

## Приёмка
- Повторный mirror-клон после force-push: `graft leaks` по всей истории — 0 находок; grep по merge-сообщениям и тегам — 0.
- `go run . all` зелёный в graft на новой истории; `git status` местного клона чистый, main == origin/main.

## Журнал
- 2026-10-29 02:27 открыт — graft-master
- 2026-10-29 02:36 в работе — graft-master
- 2026-10-29 02:36 note — graft-master: скан: 21 публичный репо (16 своих + 5 форков), 6075 коммитов, graft leaks по всем веткам и тегам + merge и теги отдельно. Термины: graft 1 (e43526f, docs/tasks строка, файл удалён 7b5ec19; только тег v2.44.1, он в proxy.golang.org с этой строкой), chooks 8 (и в HEAD), glint 2 (и в HEAD). Private names: совпадения идентификаторов (форк aiarena-web, savanna, chooks — общие слова) и в glint 768 идентификаторов рабочего кода, в HEAD тоже. Бэкап старой истории graft: mirror scratchpad scan/graft.git. План graft: filter-repo --replace-text на свежем mirror, force-push веток и тегов, local reset на origin/main
- 2026-10-29 02:41 note — graft-master: graft: filter-repo --replace-text (1 строка), force-push веток и тегов 02:4x CET: origin/main bee6978→23b47f9, затронуты теги v2.44.1..v2.50.3 (деревья кроме v2.44.1 побайтно прежние); local reset, дерево Сна switch --detach на 23b47f9 (его правка CLAUDE.md цела), gc старых объектов; ссылки на старые хеши в docs/tasks — коммит 42b6e45. Проверка: свежий клон, graft leaks по main и всем тегам — 0, сообщения — 0. chooks и glint переданы их мастерам (живые ветки на старой истории), итог от них к git-admin. Остаток вне git: proxy.golang.org держит v2.44.1 со строкой; GitHub отдаёт старые коммиты по SHA до своей GC
- 2026-10-29 02:41 ждёт — graft-master — итоги chooks-master и glint-master через git-admin; затем независимая проверка свежими клонами
- 2026-10-29 03:03 note — graft-master: дополнение (по находке chooks-master): refs/pull/* в мирроре: graft 0 (ls-remote), glint 2 (PR 4 head/merge) — graft leaks 0 находок, chooks 11 с именами — удаляет только поддержка GitHub. chooks переписан chooks-master: main 3f38ae6→b7a094f, хеши 5192474
- 2026-10-29 03:39 на проверке — graft-master — graft: свежий клон, graft leaks main+теги 0 (мой замер, исполнитель тоже я — нужна чужая проверка); glint и chooks проверены мной независимо: --branches --tags 0 терминов (587 и 150 коммитов); refs/pull: glint 65 коммитов / 2 термина, chooks 20 / 17 — только поддержка GitHub; proxy.golang.org — вне git
