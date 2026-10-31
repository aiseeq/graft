# Ревизия замысла stats, очереди, кэша и мелких фич (обход design Сна 31.10)

Статус: в работе
Исполнитель: graft-master

## Вход
- Задание Сна, обход design 31.10 (через dream-master; итог — git-admin, копия dream-master). 12 пунктов, ключевое Сон проверил.

## Что сделать

## Приёмка
- По каждому пункту: сделано (коммит, тест, замер до/после) или отдано хозяину/отложено с причиной.

## Что сделать (по пунктам задания)
1. Причину красного прогона не печатает ни одна команда: `graft stats --failures [--task] [--since]` — последние красные с причиной; ссылка в строке отказа гейта.
2. Повтор той же причины на том же дереве (22 из 190 красных гейтов 30.10): на красном печатать «та же причина, что N мин назад, K-й раз».
3. ~26% причин бесполезны (27 пустых, 14 — только сводка lint-changed, 7 начинаются с `warning: result cache off`): брать из упавшего шага, пропуская строки graft и warning:.
4. disk_profile пуст всегда, read_mb/write_mb/peak_write_mbs никто не читает — решать с aguard-master после проверки smart-admission 01.11 13:00 (ждёт).
5. `graft stats --tests --all --since 7d` — 25.6 с и 967 МБ RSS: сводка потоком (tests.go:92).
6. Промахи кэша не пишутся (16% попаданий): запись cache-miss с изменившейся частью ключа.
7. Ожидание без «за кем стояли»: поле waited_behind; ожидание за головой, ждущей тишины, писать как quiet, не queue.
8. Допуск бойца держит общую очередь 60 с (21% ожидания гейтов 30.10) — вместе с llmark-master.
9. Пороги машины в двух местах (busy и graft config) — у akit-master; graft — сторона приёма.
10. help.go:47: нет `flags mute --in` и `deploy --glint-ticket`.
11. К сведению: встроенный gofmt повторяет шаги gofmt проектов — строкой, не срочно.
12. Факт: класс ложных падений на PC закрыт v2.58.0 — без действий.


## Журнал
- 2026-10-31 02:56 открыт — graft-master
- 2026-10-31 02:56 в работе — graft-master
- 2026-10-31 03:02 note — graft-master: п.10 сделан в main: help.go flags mute [--in subject|body|key], deploy [--glint-ticket KEY] (в коммит вместе с gate-parallel-tests).
- 2026-10-31 03:06 note — graft-master: Бойцы Luna: п.1–3 — 1031-0303-feat-stats-failures (бриф /home/aisee/.cache/worktrees/graft-measure/brief-stats-failures.md); п.5 — 1031-0305-perf-stats-tests-stream (бриф brief-stats-tests-stream.md, мера stream-accept.sh: база v2.62.0 на снимке 15.0 с / 1 020 988 КБ, порог 200 000 КБ, тот же вывод). п.8 — данные и предложение отправлены llmark-master 03:10; сторона graft (пропуск гейтов мимо удержания --settle) — по его ответу. п.9 — у akit-master. п.4 — ждёт проверки smart-admission 01.11 13:00 вместе с aguard-master. п.11, п.12 — без действий (к сведению). п.6–7 — следующим бойцом, когда разгрузится машина.
- 2026-10-31 03:06 note — graft-master: п.6–7 — боец Luna 1031-*-feat-stats-waits-misses (бриф brief-stats-waits-misses.md).
- 2026-10-31 03:45 note — graft-master: 03:58 п.1–3 ревью раунд 1 бойцу: полный разбор stats.jsonl на каждом упавшем кадре (2.5 с, 137 МБ), lint-changed в ядре, формат минут, таблица; /home/aisee/.cache/worktrees/graft-measure/review-stats-failures-1.md
- 2026-10-31 04:23 note — graft-master: 04:35 п.1–3 закоммичены в feat/stats-failures 07b3ae1 (ревью раунд 2 принят); п.6–7 ревью раунд 1 бойцу: Previous читает весь passed (4987 файлов, 20 МБ) на промахе, quiet-колбэк = полный замер машины на опрос, счёт опросами, копия AcquireMode
- 2026-10-31 04:49 note — graft-master: 04:49 п.1–3 влиты 0fde57f, v2.63.0
- 2026-10-31 10:02 note — graft-master: 05:0x п.5 влит f77b81f: на снимке 15.0 с/1 020 988 КБ → 8.04 с/181 740 КБ, вывод тот же (stream-accept)
