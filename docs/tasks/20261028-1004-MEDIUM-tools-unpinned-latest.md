# graft tools keeps stale binaries of unpinned tools

Статус: готово
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- После снятия пина в tools.go_install `graft tools` считает подходящим любой стоящий бинарь и не обновляет его до последней версии; пины сняты по норме dev-process, поэтому у всех остаются старые версии (факт work-admin 28.10 со слов мастера рабочего проекта, не воспроизведено). Воспроизвести, решить, как graft tools сверяет непиненный инструмент с последней версией.

## Приёмка
- Тест: непиненный go_install со старой сборкой → graft tools сообщает отставание, `graft tools install` ставит последнюю.

## Журнал
- 2026-10-28 10:04 открыт — graft-master
- 2026-10-28 11:33 в работе — graft-master
- 2026-10-28 11:33 note — graft-master: воспроизведено: buildProblem сверяет версию только при пине, check/expect — только expect. Репро tools_latest_integration_test.go (worktree ~/.cache/worktrees/graft/tools-latest, sha256 539ef8f4…): v0.1.0 при v0.2.0 в прокси — «ok», tools install не меняет. Решение: непиненный go_install сверять с @latest модуля (go list общий с gomods), (devel)/+dirty — как сейчас, офлайн — предупреждение, сеть только в graft tools/init. Бриф — scratchpad graft-master tools-latest/brief.md; боец Luna, багфикс-по-тикету.
- 2026-10-28 11:34 note — graft-master: задание бойцу (Luna), приёмка: sha теста + graft gate в дереве бойца.
- 2026-10-28 11:50 note — graft-master: влито c78d4d6 (боец, ревью мастера без правок). Дальше: релиз, install, независимая проверка, итог work-admin.
- 2026-10-28 12:05 note — graft-master: v2.47.0 выпущен и установлен (all зелёный); независимая проверка суб-агентом идёт.
- 2026-10-28 12:21 note — graft-master: независимая проверка (суб-агент, v2.47.0): FAIL — сборка коммита после тега (псевдоверсия v0.2.1-0.…) считалась отставшей от v0.2.0 и откатывалась; на машине ~/bin/glint v1.83.8-0.… против latest v1.83.7 — tools install откатил бы glint. Фикс: сравнение по semver (x/mod/semver), юнит-случаи псевдоверсий.
- 2026-10-28 12:29 note — graft-master: v2.47.1 (5b4c18c) выпущена и установлена (laptop+pc, sha 091f47df); повторная независимая проверка — суб-агент 12:30
- 2026-10-28 13:27 готово — verify-subagent: independent verify v2.47.1 (subagent 12:33): older than latest -> built from…latest, install raises; (devel)/+dirty/pseudo after tag/newer rc -> ok; work-admin 12:53: all 8 ~/work projects ok on v2.47.1
