# cache: tree пропускает файл version.sync целиком — изменение не в поле версии не сбрасывает кэш

Статус: в работе
Исполнитель: graft-master

## Вход
- git-admin 01.11 (Сон, обход design п.5): /home/aisee/.claude/improve/runs/2026-11-01/round-design.md стр. 16 и 67. Проба: пустой репо, lock.json {version:"1.0.0",dependency:"old"} в version.sync по ключу version, задача verify с cache: tree печатает файл; после смены dependency на new второй запуск — "skipped (cache)".
- Реальный потребитель: проект с frontend/package-lock.json в version.sync.

## Что сделать
- Ключ кэша tree: файл из version.sync входит в ключ с вырезанными (нормализованными) только версионными полями; любое другое изменение сбрасывает кэш, чистая смена версии (bump) — нет.

## Приёмка
- Тесты на синтетическом lock.json: (a) смена dependency → задача запускается; (b) смена только version → skipped (cache). (a) красный на 1363fd0.
- Повтор пробы git-admin на новом бинаре: второй запуск печатает new.
- go run . all зелёный. Итог git-admin: ключ тикета, коммит, повтор пробы.

## Журнал
- 2026-11-01 03:34 открыт — graft-master
- 2026-11-01 03:34 в работе — graft-master
- 2026-11-01 04:16 note — graft-master: Боец Luna 1101-0335 принят. Ревью мастера: в ключ добавлен CommonDir репозитория — смена замысла (кэш делится между репо по содержимому) из-за изъяна моей пробы (новый tmp-репо с тем же содержимым брал запись прошлого запуска) — убрано; проба: уникальный токен на запуск. Проба: старый graft — FAIL (dependency → skipped), новый — ok дважды подряд. Тесты TestCacheVersionSync (интеграционный, 4 шага) и TestCacheVersionSyncKey. Коммит 6e78633 на cache-version-sync-content. Дальше — независимая проверка.
