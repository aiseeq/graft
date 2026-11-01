# Тяжёлые задачи graft (exec --heavy, гейт, тесты) — IOWeight=10 при постоянном iocost

Статус: в работе
Исполнитель: graft-master

## Что сделать
Задание лида 01.11 (SendMessage): на ноуте с 01.11 14:21 постоянно включён iocost, веса: app.slice 100, agents.slice 40. Тяжёлые задачи graft должны идти с IOWeight=10, как ферма llmark (f123363) и бойцы worker.

План:
- замер: cgroup и IOWeight живых heavy-задач graft (гейт, тесты, exec --heavy) — от кого наследуют;
- grep: graft сам вес не ставит (systemd-run/IOWeight в коде нет, 01.11);
- как ставит llmark (f123363) — тот же механизм, без своего;
- реализация, тест, установка.

Чем проверить: `systemctl --user show <scope> -p IOWeight` у живой heavy-задачи graft → IOWeight=10; у лёгких задач — без изменений.

## Приёмка

## Журнал
- 2026-11-01 14:21 открыт — graft-master
- 2026-11-01 14:21 в работе — graft-master
- 2026-11-01 14:22 note — graft-master: Замер 14:22: живые heavy — 5 из 8 в worker-*.service (app.slice, IOWeight не задан → 100; у юнита MemoryMax=12G, KillMode=mixed), 3 в agent-*.scope (agents.slice=40, у scope не задан → 100). graft вес не ставит. io.weight делит только между соседями: heavy в юните бойца получает вес юнита (бойцам 10 ставит llmark-master) — выносить heavy из юнита бойца нельзя: ушёл бы из-под MemoryMax=12G и остановки юнита. В agent-scope heavy делит agents.slice с консолями агентов на равных. Проба: systemd-run --user --scope --slice=agents.slice -p IOWeight=10 -p BindsTo=<scope вызвавшего> -p After=… — io.weight 10, BindsTo принят (systemd 259). План: heavy из agent-scope (agents.slice, без лимитов юнита) — в свой scope с IOWeight=10 и BindsTo; из сервисов (worker) — не трогать, вес даёт юнит. До кода — противник.
- 2026-11-01 14:23 note — graft-master: Противник: worker ask terra, бриф /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-graft/004ae628-6547-47e2-96e1-dc2539f82859/scratchpad/adversary-io-weight.md (scope вызывающего → свой scope IOWeight=10 + BindsTo; service — не трогать).
- 2026-11-01 14:25 note — graft-master: Противник (adversary-io-weight.answer.md рядом с брифом): принято — #2 сумма весов многих тяжёлых → общая подслайса <P>-graft.slice с IOWeight=10 (проба transient slice 14:3x: io.weight 10); #5 протёкший GRAFT_ADMITTED → решение по cgroup, не по env; #6 ошибка переноса — ошибка задачи; #7 лимиты scope → не переносить при memory.max/high/cpu.max (сейчас у 17 agent-scope лимитов нет). Вне графта: #3 agents.slice 40 vs app.slice 100 — настройка слайсов лида; #4 сервисы не-бойцы остаются со своим весом; #8 commit/push после гейта под весом 10 — приемлемо. Бриф Luna: /home/aisee/.cache/claude-tmp/claude-1000/-home-aisee-git-graft/004ae628-6547-47e2-96e1-dc2539f82859/scratchpad/brief-heavy-io-weight.md, дерево /home/aisee/.cache/worktrees/graft/heavy-io-weight.
- 2026-11-01 14:25 note — graft-master: Боец Luna 1101-1425-heavy-io-weight запущен.
- 2026-11-01 14:44 note — graft-master: Сдача Luna 14:43 (без коммита, гейт зелёный). Ревью мастера 14:45: живой exec --heavy из agent-scope падает — нет cpu.max (контроллер cpu не включён в agents.slice) → ошибка; плюс потеря ошибки busctl в ensureSlice и обнуление reserveRelease кадра во вложенном вызове. Доводка: worker continue, бриф brief-heavy-io-weight-r2.md.
