# заметка о выкатке и перевод — только за коммиты, меняющие поведение

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема: замер потребителя 2026-10-29: `graft deploy` на тестовую среду написал «Выкачено…» в задачу, чей единственный коммит в выкатке — `docs(…): KEY …` (переписан комментарий в коде и поднят VERSION); QA принял задачу за выкаченную и нашёл старое поведение. Задача была в статусе «на паузе» (не to-do, не done): `noteable` её не отсекает.
- Метод: deliveredKeys (internal/app/deploy.go) — ключ, у которого все коммиты выкатки типа docs, test, style, ci (Conventional Commits), не доставлен: нет заметки, fix version, перевода; строка в вывод.
- Статус «на паузе» — уже есть `release_notes.jira.skip_statuses` (README «put aside by hand: Blocked, On Hold»): потребителю добавить свой статус в конфиг; в graft не дублировать.
- Если не делать: такой коммит у задачи In Progress ещё и переводит её в Testing.

## Что сделать
- message: экспорт типа Conventional Commits из темы коммита (одна регулярка с CheckConventional).
- deliveredKeys: ключи только из docs/test/style/ci-коммитов — отдельный список, noteKeys печатает «release note for KEY not posted: its commits in this deploy are docs, test, style or ci only».
- README: абзац о том, какие коммиты доставляют задачу.

## Приёмка
- Тест: выкатка с `docs(x): PROJ-1 …` и `fix(y): PROJ-2 …` → заметка только у PROJ-2, строка про PROJ-1; ключ с docs и fix — доставлен. Красный на main, зелёный после; `go run . all`.

## Журнал
- 2026-10-29 12:03 открыт — graft-master
- 2026-10-29 12:03 в работе — graft-master
- 2026-10-29 12:08 note — graft-master: сделано мастером (правка ~30 строк, бойцу дольше брифа): message.ConventionalType, deliveredKeys отделяет quiet, README; TestDeployReleaseNotesSkipQuietCommits красный на main, зелёный после; go run . all — пакеты ok
