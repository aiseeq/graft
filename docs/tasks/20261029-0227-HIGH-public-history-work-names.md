# История публичных репо aiseeq/* без имён и данных рабочих проектов

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
