# graft leaks: merge-коммиты не проверяются (--no-merges): строка, внесённая только в merge, и сообщение merge

Статус: готово
Исполнитель: graft-master

## Что сделать

## Приёмка
- Тест на синтетике: две ветки, конфликт разрешён строкой с тестовым термином только в merge, второй термин в сообщении merge → `graft leaks --all` называет оба, код ≠0; обычные изменения второй ветки не считаются новыми (--remerge-diff или --cc; diff против первого родителя не годится — выдаст всю вторую ветку).
- Merge без конфликта и без новых строк — без находок, код 0.
- Merge, который git не умеет разобрать (octopus, git старше 2.36), — ошибка с именем коммита, не молчаливый пропуск.
- Обычные коммиты и проверка staged при `graft commit` — как прежде (TestLeaksScansHistory, TestLeaksTakesAnyRevisions зелёные).

## Что
- Находка F1 критика ТЗ (тикет spec-as-built): internal/app/leaks.go:133 — rev-list --no-merges; diff merge и его сообщение не сканируются. ТЗ и README.md (раздел leaks) обещают каждый коммит.
- internal/leaks/scan.go:35 git diff-tree для merge без выбора родителя даёт пусто.


## Журнал
- 2026-10-31 17:45 открыт — graft-master
- 2026-10-31 20:57 note — graft-master: 20:57 боец Luna 1031-2057-leaks-merge-commits, дерево /home/aisee/.cache/worktrees/graft/leaks-merge-commits; бриф и мера — scratchpad graft-master leaks-merge/{brief.md,accept.sh}. Не новый механизм (исправление охвата проверки) — противник не нужен.
- 2026-10-31 21:17 note — graft-master: 21:3x Luna приняла (16 мин): обход без --no-merges, merge — git diff-tree --remerge-diff, README Git 2.36. Ревью мастера: git молча пропускает octopus в remerge-diff (warning, код 0) — добавил явную ошибку с именем коммита и тест; родители читаются одним rev-list --parents вместо git на коммит. Влито патчем на main (go.mod/go.sum бойца — подъём akit гейтом, не взят).
- 2026-10-31 21:21 note — graft-master: Приёмка дополнена (merge без конфликта, octopus/старый git — ошибка, обычные коммиты как прежде). Независимая проверка — свежий суб-агент a3de706, бриф scratchpad leaks-merge/verify-brief.md. Параллельно go run . all перед релизом.
- 2026-10-31 21:26 готово — verifier-opus (session graft-master): Свежий суб-агент a3de706 на бинаре из 20ed2e1, скрипты scratchpad leaks-verify/{scenarios,edges,edges2}.sh: конфликт с термином + термин в сообщении — оба найдены, код 1, правка второй ветки не выдана; чистый merge — код 0; --no-commit с правкой — найдено; octopus и git без --remerge-diff — ошибка с именем коммита; строка из коммита ветки — одна находка в её коммите (и при --theirs); мутации (--no-merges, без ветки octopus) — TestLeaksScansMergeCommit FAIL; крайние: не-ASCII, modify/delete, rename/rename, add/add, zdiff3, unrelated, -s ours — ловит.; glint: 12 findings, debt glint-debt-reserve
