# Заморозка приёмки релиза: graft deploy freeze, отказы requires-цели, advisory-задачи

Статус: в работе
Исполнитель: graft-master

## Вход
- Задание work-admin 30.10 ~10:00 CET (решение пользователя: `user-word /home/aisee/.claude/projects/-home-aisee-work/06e8c51e-b235-4bc8-93c4-a75b7dfac7c2.jsonl 'Давай А'`, 09:56:05; дополнение C: `… 'не требовать чтобы glint проходил'`, 09:56:35). Пилот — рабочий проект (имя не писать), заданием его мастеру через work-admin после релиза graft.
- Зачем: во время приёмки релиза заказчиком цель приёмки (UAT) пять раз перекатили не из релиза, в версию Jira после закрытия спринта добавили задачи, последнюю перекатку вызвал новый релиз glint (high на гейте prod).
- На идущий в пилоте релиз из release-ветки не накатывать: заморозка — только опция конфига, без неё поведение прежнее.

## Что сделать

## Приёмка
- `freeze_accept_test.go` (мой, до реализации): ветка и тег, отказ второй заморозке, отказы requires-цели (ветка, чужая задача, добавленная после заморозки не-Bug), Bug проходит, `--past-freeze` с журналом, advisory на deploy и на гейте ветки при отказе на trunk, конец заморозки деплоем prod и `thaw`, строки на странице релиза.
- `go run . all` зелёный; независимая проверка суб-агентом по этому тикету; diff и сообщения без внутренних имён.

## Что сделать (схема)
Конфиг на цели релиза (prod), рядом с `requires`:
```yaml
deploy:
  targets:
    prod:
      requires: uat                  # заморозка держит эту цель
      freeze:
        branch: "release/{version}"  # по умолчанию; {version} — имя версии Jira без approved_release.prefix
        types: [Bug]                 # задачи этих типов можно добавить в версию после заморозки
        advisory: [glint]            # задачи (deps деплоя, гейт коммита на замороженной ветке), чей сбой — предупреждение
```
1. `graft deploy freeze <target> <версия Jira>` — на trunk (HEAD = remote trunk, дерево чистое): ветка `release/<v>` от HEAD (есть на remote — берётся она), push во все remotes; аннотированный тег `freeze/<target>/<v>` на её вершине, в сообщении — версия, ветка, время (GRAFT_NOW), кто; push во все remotes. Вторая заморозка той же цели при действующей — отказ. `graft deploy freeze <target>` — показать действующую. Версии нет в Jira (project_keys) — отказ.
2. Пока тег есть на remote (`git ls-remote`), `graft deploy <requires-цель>` отказывает, до deps и sudo:
   - HEAD не на ветке заморозки (и не на `deploy.hotfix.branches`, как у hold);
   - коммит из `<prod deployed>..HEAD` (merge — мимо; revert, ignore_paths и attribute — как в approved_release prod) не называет задачу с fix version = замороженная (или подзадачу, чей родитель в ней);
   - задача, пропущенная прошлым пунктом, получила эту fix version после заморозки (последнее добавление в changelog, поле Fix Version), и её тип (или тип родителя, через которого она прошла) не в `types`.
   `graft deploy check` показывает те же отказы.
3. Обход: `graft deploy --past-freeze '<причина>' <цель>` — предупреждение, запись в журнал заморозки (тег `freeze-log/<target>/<v>/<время>-bypass`, в сообщении цель, коммит, причина), строка на странице релиза.
4. C: на деплое коммита замороженной ветки (на requires-цель и на prod) сбой deps-задачи из `advisory` — предупреждение `graft: <task> failed, advisory while <версия> is frozen`, деплой идёт; запись `freeze-log/…-advisory` (цель, коммит, задача); строка на странице релиза. На гейте `graft commit` на замороженной ветке (локальный тег) — так же. Гейт на trunk и деплой не с ветки — как раньше.
5. Конец: успешный деплой prod коммита замороженной ветки (после записи deployed_sha и страницы релиза) удаляет тег заморозки и её `freeze-log` на всех remotes. `graft deploy thaw <target> --reason '…'` — то же руками (релиз отменён).
6. README (раздел Release windows and hotfixes: подраздел Freeze), таблица команд, `graft deploy -h`.


## Журнал
- 2026-10-30 09:59 открыт — graft-master
- 2026-10-30 09:59 в работе — graft-master
- 2026-10-30 10:00 note — graft-master: work-admin 30.10 ~10:10 CET принял: 1) hotfix-ветки проходят заморозку, но выкатка hotfix на замороженную цель пишется в freeze-log и на страницу релиза, как --past-freeze, причина — имя hotfix-ветки; 2) advisory на гейте graft commit замороженной ветки — да (это C); 3) ставить сразу — да, регресс: конфиг без freeze на deploy uat и prod как раньше (существующие deploy/release тесты без правок).
- 2026-10-30 10:05 note — graft-master: Приёмка написана: /home/aisee/.cache/worktrees/graft/feat-release-freeze/freeze_accept_test.go (6 тестов, регресс без freeze — TestNoFreezeKeepsDeploysAsBefore), красные на отсутствующем ключе/команде, фикстура до заморозки проходит. fakeJira: changelog Fix Version (fixChanges) — в main. Дальше: боец Luna в feat/release-freeze.
