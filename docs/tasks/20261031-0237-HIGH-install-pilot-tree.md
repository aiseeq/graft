# graft install: гейт пилота правит живое дерево пилота (go.mod)

Статус: в работе
Исполнитель: graft-master

## Вход
- Задание Сна 31.10, обход hygiene (через dream-master, итог — git-admin; отчёт /home/aisee/.claude/improve/runs/2026-10-31/round-hygiene.md).
- install 30.10 17:41:06 (stats.jsonl run da47f0b394dae522) → гейт пилота e486062265c85e18 → go.mod пилота изменён 17:41:16 (v2.60.0 → v2.61.0) через raiseOwnModules, коммита нет. Повтор: a17e67d пилота 29.10. Обратная сторона: пилот проверяет незакоммиченную работу его мастера (отказ install 28.10 02:59, тикет install-task).

## Что сделать
- Пилот при install не трогает рабочее дерево пилота и не зависит от его незакоммиченного состояния (например, гейт на временном дереве из HEAD пилота с уборкой).
- Подъём модулей при graft commit (decisions.md own-modules-latest) не менять.
- Остаток в дереве пилота (go.mod/go.sum v2.61.0) не трогать.

## Приёмка
- Тест: после install с пилотом `git status` пилота пустой, если был пустым; незакоммиченная правка пилота не влияет на итог install.
- go run . all зелёный.

## Журнал
- 2026-10-31 02:37 открыт — graft-master
- 2026-10-31 02:37 в работе — graft-master
- 2026-10-31 03:03 note — graft-master: Боец Luna 1031-0303-fix-install-pilot-tree (багфикс-по-тикету): бриф /home/aisee/.cache/worktrees/graft-measure/brief-install-pilot-tree.md, дерево /home/aisee/.cache/worktrees/graft/fix-install-pilot-tree, приёмка — graft gate + go test -run Pilot с PASS. После — ревью diff и независимая проверка.
