# заметка о выкатке и перевод — только за коммиты, меняющие поведение

Статус: готово
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
- 2026-10-29 12:23 note — graft-master: ревью суб-агента 37f9c45: merge-коммит с ключом в теме обходил фильтр; строка «no work item keys» при одних quiet; нет юнит-теста ConventionalType — всё исправлено: deliveredCommit.merge (%P), ключ merge доставлен, только если его не называют quiet/revert-коммиты выкатки; тест с --no-ff merge красный без правки; go run . all EXIT 0
- 2026-10-29 12:30 note — graft-master: повторное ревью 703fd33: ключ только в имени ветки при безключевых docs-коммитах ветки снова доставлялся; README «subject» уже поведения — исправлено: merge решает по коммитам merge^1..merge (есть не-quiet, не-revert → доставлен), тест PROJ-9 (docs) и PROJ-10 (fix) по --no-ff, красный без правки; go run . all EXIT 0
- 2026-10-29 12:38 note — graft-master: третье ревью 718bd22: A и B закрыты; остаток (был и раньше): merge ранее выкаченной ветки повторно доставлял её ключ — диапазон merge с ^prev, TestDeployReleaseNotesMergeOfDeployedBranch красный без правки (2 заметки), зелёный после; go run . all EXIT 0
- 2026-10-29 12:45 готово — reviewer-opus: независимое ревью суб-агента Opus в 3 раунда (37f9c45, 703fd33, 718bd22): все находки исправлены, последняя (^prev) — по его же предложению, тест красный без правки; TestDeployReleaseNotes*, TestConventionalType, go run . all EXIT 0 на 3c30a34; сложность deliveredKeys вынесена в goingOut/undoneBy/mergeChanges
