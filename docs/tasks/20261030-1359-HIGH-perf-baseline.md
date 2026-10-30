# Референс скорости graft (perf-baseline-1030, этап 1): graft.tsv и graft.md по FORMAT.md

Статус: готово
Исполнитель: graft-master

## Вход
- git-admin 30.10 ~13:58 CET: задание лида perf-baseline-1030, этап 1 (/home/aisee/docs/tasks/20261030-1136-MEDIUM-perf-baseline-1030.md; тикет git-admin /home/aisee/git/docs/tasks/20261030-1137-HIGH-perf-baseline-git.md). graft — второй после пилота akit.
- Слово пользователя: `user-word ~/.claude/projects/-home-aisee/d753e3a8-1750-4371-bdef-a4263a5bf0f7.jsonl 'собирать статистику работы всех'` (30.10 11:34 CET).
- Формат: /home/aisee/.claude/improve/state/perf/FORMAT.md (13:25: референс — худшее суточное за 7 дней). Образец: akit.tsv, akit.md там же.
- Поправки пилота: мера ожидания cli — `tl --all --since 24h --wait 'PROG [SUB]' --q 95` (akit v1.14.1, флаги не различает); `команда` без `graft exec --heavy` (ночь даёт 120 с); wall фиксированного запроса шумит до ×1,8 — худшее из прогонов, условия `busy` в .md; очередь heavy — отдельный путь (`wait_s` stats.jsonl).

## Что сделать
1. Пути graft, которые агенты зовут чаще и дольше ждут: commit, gate, test, exec, worktree, stats, deploy и др. — по `tl --wait` за 7 дней (суточные p95) и по stats.jsonl (`wall_s - wait_s` задач, `wait_s` очереди heavy).
2. /home/aisee/.claude/improve/state/perf/graft.tsv и graft.md строго по FORMAT.md; кандидаты ×2 — только назвать с замером.
3. Коммит в claude-config (`git --git-dir=$HOME/.claude-config.git --work-tree=$HOME`, add явными путями); итог git-admin строкой: пути файлов, число путей и сколько с командой, кандидаты ×2 с числами, что мешало в задании/FORMAT.md.

## Приёмка
- /home/aisee/.cache/worktrees/graft-measure/perf-accept.sh зелёный (формат tsv, каждая команда ≤ 60 с: одно число и код 0 либо код 3, без graft exec --heavy; разделы .md); независимая проверка суб-агентом по FORMAT.md.

## Журнал
- 2026-10-30 13:59 открыт — graft-master
- 2026-10-30 13:59 в работе — graft-master
- 2026-10-30 14:05 note — graft-master: боец Luna (разбор-данных): бриф /home/aisee/.cache/worktrees/graft-measure/brief-perf-baseline.md, приёмка perf-accept.sh, дерево /home/aisee/.cache/worktrees/graft/perf-baseline (только чтение исходников)
- 2026-10-30 14:33 note — graft-master: graft-master: боец 1030-1405 принят приёмкой (13 путей). Моё ревью: cli:graft gate и cli:graft exec — недельный p95 вместо суточного; перепроверил 7 суток — код 3 везде, строки убраны в Пробелы, в приёмке требование cli:graft gate → job:graft task gate (моя ошибка приёмки). Кандидат ×2 дописан: heavy queue (56% wall гейта 25–30.10), gate — не кандидат с причинами. Приёмка exit 0, 11 путей. Дальше — независимая проверка суб-агентом.
- 2026-10-30 14:50 note — graft-master: graft-master: независимая проверка (суб-агент Opus): не готово. cli-строки все сверены. Майор: job-референсы по календарным суткам, команда — скользящие 24 ч (очередь: 352 против 2451 с за скользящие сутки 26–27.10, gate 116.9 против 124.1); task-p95.sh падает без TMPDIR; мелкое в .md (диапазон cli:graft, источник 116.9, n по путям, кто ждёт/почему главный, пустые строки в таблице, кандидат очереди — доля снимаемого). План: после релиза graft stats --value (тикет stats-value) job-команды → graft stats --value, референсы пересчитать по скользящим 24 ч тем же отбором, task-p95.sh удалить, .md поправить, повторная проверка.
- 2026-10-30 15:33 note — graft-master: graft-master: доработка после проверки: job-строки → graft stats --value (v2.60.0): job:graft gate 151.314, job:graft all 386.176 (graft-проект, wall-p95), job:graft heavy queue 245.232 (все проекты, wait-p95); скользящие 24 ч до 15:30 CET тем же отбором, что у команды (последние сутки совпали с командой); task/exec других проектов убраны (их скорость — в их tsv); task-p95.sh удалён; .md переписан по находкам. Приёмка exit 0 (9 путей). Повторная независимая проверка — суб-агент.
- 2026-10-30 15:53 note — graft-master: graft-master: вторая проверка — не готово (p95 job без порога 20, отбор до 29.10 описан неверно, codex, 56 %). Правка 15:55: job:graft all → wall-p50 236.203 (медиана, в большинстве суток < 20 прогонов), gate и очередь — --min-runs 20 (референсы те же), .md: отбор команды, codex только с --codex, конфликт FORMAT про очередь в cli-строках, доля очереди 51 % (4071 гейт до 30.10 15:30). perf-accept.sh exit 0. Третья проверка — суб-агент.
- 2026-10-30 16:03 готово — opus-subagent-check (session graft-master): третья независимая проверка (суб-агент Opus, бриф /home/aisee/.cache/worktrees/graft-measure/brief-check-perf-baseline.md): готово — все 9 референсов воспроизведены (cli: tl по 7 окнам до 14:06; job: stats.jsonl* отбором value.go), perf-accept.sh exit 0. Коммит claude-config d68f96e. Кандидатов ×2 нет; очередь — гипотеза под remote-heavy-share.
