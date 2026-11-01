# Ночной gocache trim без фризов машины

Статус: на проверке
Ждёт: 2026-11-01 05:00 CET: замер ночного trim 04:30
Исполнитель: graft-master

## Что сделать
`graft gocache trim --apply` (юнит graft-gocache-trim.timer, 04:30) удаляет за раз 237–571 тыс. записей (36–108 ГиБ): 29.10 351 тыс./102 ГиБ за 1м45с, 30.10 237 тыс./36 ГиБ за 2м34с, 31.10 571 тыс./108 ГиБ за 5м40с (journalctl --user -u graft-gocache-trim). Сторож фризов машины: фриз 31.10 04:35:23 2.8 с, фон ядра (writeback, коммит btrfs) записал 1691 МБ = 98% записи устройства, главная cgroup — graft-gocache-trim.service, очередь диска до 792, dirty 345 МБ; 18 случаев с gocache-trim с 29.10. IOSchedulingClass=idle не помогает: метаданные и коммит btrfs пишет ядро вне cgroup.
Нужно: удалять пачками с допуском по моментальному состоянию (decisions.md instant-not-average): перед пачкой ждать, пока dirty+writeback и давление IO (моментальные) ниже порога; размер пачки и паузы — по замеру.
Отдельно (не чинить, число в итог): какая доля записи на SSD — ~100 ГиБ/сутки в GOCACHE.

## Приёмка
- Замер ночи (или ручной прогон на тех же объёмах): фризов от trim 0 по сторожу фризов, время trim — сколько выйдет.
- Тест: пачка не начинается, пока моментальная мера выше порога (подменный /proc).
- Пороги (1000 записей, 256 MiB, Dirty+Writeback 64 MiB) по итогу замера оставлены или поправлены — с цифрами ночи (находка F6 критика ТЗ, тикет spec-as-built).

## Журнал
- 2026-10-31 10:11 открыт — graft-master
- 2026-10-31 10:11 в работе — graft-master
- 2026-10-31 10:11 note — graft-master: 10:15 боец Luna, бриф /home/aisee/.cache/worktrees/graft-measure/brief-gocache-trim-freeze.md; доля GOCACHE в записи: ~100 ГиБ/сут из ~875 ГиБ/сут nvme0n1 (12 881 ГиБ sectors written за 353 ч аптайма) ≈ 11%
- 2026-10-31 10:37 note — graft-master: 11:1x боец сдал раунд 1 (пачки ≤1000 записей/256 МиБ, Dirty+Writeback<64 МиБ, busy kind gocache-trim, syncfs, --max-wait 30m; синтетика 50k×100 КБ: 1.37 с → 5.20 с, 50 пачек). Ревью раунд 1: свод двойной классификации записи, мера meminfo в loadgate — review-gocache-trim-1.md
- 2026-10-31 10:39 note — graft-master: мёртвый параметр entry(apply=false) отдан glint-master: тикет glint 20261031-1038-MEDIUM-param-always-same-constant, находки в graft пришлёт
- 2026-10-31 11:08 note — graft-master: 11:0x влито 05f17b8 (после чистки истории; было 5963b33), v2.65.0 установлен на ноут и PC; all зелёный 10:54–11:00. Дальше — ночь 01.11 04:30: aguard cases.jsonl + journalctl --user -u graft-gocache-trim → git-admin со строкой ~11% записи SSD.
- 2026-10-31 11:08 ждёт — graft-master — 2026-11-01 05:00 CET: замер ночного trim 04:30
- 2026-11-01 05:16 note — graft-master: замер ночи 01.11 (journalctl --user -u graft-gocache-trim): старт 04:30, конец 05:08:48 — 38 мин 48 с wall, 3 мин 55 с CPU, удалено 366 630 записей / 47,4 ГиБ, оставлено 668 344 / 110,4 ГиБ, ожидание машины 15 мин 36 с. Сторож aguard: GUI-фризы (gui/slices.json) в окне 04:30–05:09 — 56 срезов, из них D (диск) 14, max 6,8 с; час до trim (03:30–04:09) — 53, D 14, max 20,0 с: trim фризов не добавил. IO-заторы (io/cases.jsonl): окно trim — 38, сумма 7,9 с, max 0,9 с; час до — 22, 7,7 с, max 2,3 с; та же ночь 31.10 со старым trim — 55, 24,7 с, max 2,8 с, 15 по ≥ 0,5 с. Пороги (1000 записей, 256 МиБ, Dirty+Writeback 64 МиБ) оставлены. Попутно: строка 'caller watcher poll: fd 1: interrupted system call' в 04:32:30 — тот же EINTR (caller-poll-eintr). Доля GOCACHE в записи SSD ~11% (10:11 31.10).
- 2026-11-01 05:16 на проверке — graft-master
- 2026-11-01 06:46 note — graft-master: Независимая проверка (суб-агент a86c55fc8f80d7022, бриф graft-measure/check-gocache-trim.md, 06:35–06:41): п.3 пороги — принят (Dirty+Writeback ≥ 64 МиБ: 261 с из 2329 в окне против 308 до — trim грязных не нагнал; из 15,6 мин ожидания на dirty ≤ ~4,5 мин, остальное busy). Дефекты: (1) без /proc/meminfo (Windows, macOS) trim --apply падал, ничего не удаляя — регрессия 05f17b8; (2) проводка ожидания в команде не покрыта (мутация 'убрать options.BeforeBatch' зелёная); (3) README: «disk pressure» — на деле весь busy-чек; (4) формулировка ночи: заторов ≥ 1 с от trim 0, но два мелких (0,9 с и 0,5 с) — писатель сам trim (76 и 70 МБ); GUI-счёт 56/53 упирался в потолок сторожа (MaxPerHour 60) — не довод; (5) CPU на запись в 4,5 раза выше старого (0,64 мс против 0,14) — 3 мин 55 с за ночь, по «имеет ли значение» не трогаю. Исправлено: (1) dirtyMeasured — вне Linux без GRAFT_PROC пачка ждёт только busy, TestTrimMemoryCalmWithoutTheMeasure; (2) TestGocacheTrimApplyWaitsForTheMachine — Dirty 900 МБ, --max-wait 1s → removed 0, remain 1, waited 1s; мутация BeforeBatch — красный; (3) README.
- 2026-11-01 06:50 note — graft-master: Исправления дефектов 1–3 — b28cad4 (main). Повторная независимая проверка исправлений — в следующей пачке проверок.
