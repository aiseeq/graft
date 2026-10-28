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
