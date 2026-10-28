# go-build cache grew 100 GB overnight

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
- Разобрать рост ~/.cache/go-build 27→28.10 (83→184 ГБ за ночь) против оценки ~26 ГБ/сут (backlog.md:34), решить, прав ли порог ~200 ГБ; итог git-admin (просьба лида 28.10 ~09:50).

## Приёмка
- Источник прихода назван замером по кэшу (find -newermt, export data); решение и порог записаны в backlog.md:34; glint-master и aguard-master получили свои части.

## Журнал
- 2026-10-28 09:47 открыт — graft-master
- 2026-10-28 13:08 note — graft-master: glint-master 13:1x: разбиение pkg/rules/patterns переделывается (первый заход Luna потерял ~380 doc-блоков), заход Opus, итог с замером — сегодня к вечеру.
- 2026-10-28 13:50 note — graft-master: glint-master 13:50: split merged a094825, glint installed; per edit 11-17 MB / 3.6-3.9 s / 0.5 GB RSS (was 80-97 MB / 39 s / 1.4 GB); consumers 0 diffs on 22 projects. Independent verify subagent 13:5x; then tk done and result to git-admin + aguard-master (backlog.md:34 estimate).
- 2026-10-28 13:54 готово — verify-subagent: source measured 09:45 (glint patterns archives ~73 of ~100 GB/night); split a094825 merged; independent verify 13:58: one rule edit writes 11-17 MB (was 80-97), build 4-4.5 s, 0.53 GB RSS; backlog.md:34 updated; cache 200 GB at 13:45, /home free 360 GB (stop-line 150)
- 2026-10-28 15:41 note — graft-master: 15:4x повторный замер по письму git-admin: mtime-приход 11,5 ГБ/ч включает переиспользование (Go трогает mtime при использовании), по birth time (stat %W) за час до 15:37 родилось 8,2 ГБ (3,3 ГБ — старые записи). Сутки по рождению: ~142 ГБ, из них ночь 27.10 21–03 ч ~96 ГБ (patterns, закрыто); 28.10 09–11 ч ~2 ГБ/ч, 12–15 ч 5,4–7,0 ГБ/ч. По пакетам (родившиеся ≥1 МБ за час): glint 2,7 ГБ (подпакеты patterns/* по ~26 версий в час — правки helpers в worktree test-helpers пересобирают все правила × варианты), sc2go 1,1, graft 0,9, закрытые проекты ~1,7, чужие зависимости <0,3. Отдельно: без -trimpath каждый worktree/клон держит свои архивы собственных пакетов (тот же коммит в другой папке: 33/33 export разные, с -trimpath 32/33 общие).
- 2026-10-28 15:43 note — graft-master: 15:4x решение git-admin: (а) ничего не меняем; backlog.md:33 обновлён (дневной приход, полка 300–500 ГБ, стоп-линия, условие для своей чистки).
