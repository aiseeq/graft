# commit sweeps in paths written during the gate

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
- `graft commit` делает `git add -A` после гейта и уносит пути, появившиеся в дереве за время гейта от второго писателя (факт git-admin 28.10: коммит 5a9d251 в другом репо, 10:06:24 CET, чужая правка 10:05:55 во время гейта). Мера у источника: запомнить состав грязного дерева (пути и их содержимое) на старте; к концу гейта новые или изменённые пути, кроме своих (modules.latest go.mod/go.sum, правки fmt), — громкий отказ с перечнем путей.

## Приёмка
- Тест: во время гейта шаг пишет новый файл в дерево → commit отказывает и называет путь; правки modules.latest проходят.

## Журнал
- 2026-10-28 10:18 открыт — graft-master
- 2026-10-28 11:24 в работе — graft-master
- 2026-10-28 11:24 note — graft-master: намерение изменено: не отказ, а «оставить вне коммита». Перепись .graft.yaml потребителей: гейты пишут файлы законно (gofmt -w, goimports -w, go mod tidy — у нескольких потребителей, graft в их числе), modules.latest пишет до первого add -A. Правило: в коммит — пути, грязные на старте (индекс против HEAD после первого add -A), в виде, в каком их оставил гейт, со строкой «<path> changed while the gate ran»; путь, изменённый только во время гейта, — не индексируется, остаётся в дереве, строка «left out <path>: …»; следующий коммит берёт его со старта. То же в amend (internal/app/other.go). Остаток риска: чужая правка уже грязного пути во время гейта уходит в коммит, но со строкой.
- 2026-10-28 11:24 note — graft-master: репро: worktree ~/.cache/worktrees/graft/late-paths (ветка late-paths от 132e5a9), commit_late_paths_integration_test.go sha256 ab69cf99…, оба теста красные по нужной причине (late.txt и правка clean.txt в коммите, строк нет). Бриф — scratchpad graft-master late-paths/brief.md; задание бойцу Luna, класс багфикс-по-тикету.
- 2026-10-28 11:24 note — graft-master: задание бойцу 1028-1124-late-paths (Luna, приёмка: sha теста + graft gate в дереве бойца). Итог — worker wait.
- 2026-10-28 11:53 note — graft-master: влито c53b67c (боец + правки ревью). Дальше: релиз, install, независимая проверка, итог git-admin.
- 2026-10-28 12:05 note — graft-master: v2.47.0 выпущен и установлен (all зелёный); независимая проверка суб-агентом идёт.
- 2026-10-28 12:21 note — graft-master: независимая проверка (суб-агент, v2.47.0): FAIL — git add/mv/rm второго писателя во время гейта уходят в коммит при строке «left out». Фикс: после гейта индекс возвращается к дереву старта (read-tree), затем индексируются сохранённые пути; тест TestCommitLeavesOutWhatAnotherWriterStagedDuringTheGate красный без фикса, зелёный с ним.
- 2026-10-28 12:29 note — graft-master: v2.47.1 (5b4c18c) выпущена и установлена (laptop+pc, sha 091f47df); повторная независимая проверка — суб-агент 12:30
- 2026-10-28 12:35 note — graft-master: Проверка v2.47.1: заявленное PASS (t1c, t1d, *?-имена, удаления, падение гейта, без HEAD, amend, git mv). Новые дефекты: файл→каталог (x/y коммитится, хотя left out), amend считает старт от HEAD а не HEAD^ (форматирование файла правимого коммита теряется), гейт откатил всё → коммит одного бампа / сырая ошибка git. Спец-тесты commit_start_set_integration_test.go (sha bb21dd36…, красные 3/3), боец Luna 1028-1234-start-set, worktree start-set.
- 2026-10-28 12:54 note — graft-master: Боец Luna 1028-1234-start-set: 'не справился' — по вине спец-теста (TestCommitWhoseGateUndoes…: empty.txt не входил в первый коммит, боец верно указал). Тест исправлен (sha eb9a8432…), код бойца зелёный; упростил gateLeftHead (без HEAD — false, без второй копии empty tree). Перенёс diff в main через git apply.
- 2026-10-28 13:09 note — graft-master: Проверка v2.48.0 (суб-агент 13:06): 1–3 PASS. Новый дефект: amend, чей гейт опустошает правимый коммит → сырая ошибка git, индекс не восстановлен (и на ошибках checks/commit amend). Исправлено: stagedIs(base) → errGateEmptiedAmend, restoreIndex на поздних шагах amend; тест TestAmendWhoseGateEmptiesTheCommitHasNothingToAmend.
