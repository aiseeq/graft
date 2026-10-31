# Ночной gocache trim без фризов машины

Статус: в работе
Исполнитель: graft-master

## Что сделать
`graft gocache trim --apply` (юнит graft-gocache-trim.timer, 04:30) удаляет за раз 237–571 тыс. записей (36–108 ГиБ): 29.10 351 тыс./102 ГиБ за 1м45с, 30.10 237 тыс./36 ГиБ за 2м34с, 31.10 571 тыс./108 ГиБ за 5м40с (journalctl --user -u graft-gocache-trim). Сторож фризов машины: фриз 31.10 04:35:23 2.8 с, фон ядра (writeback, коммит btrfs) записал 1691 МБ = 98% записи устройства, главная cgroup — graft-gocache-trim.service, очередь диска до 792, dirty 345 МБ; 18 случаев с gocache-trim с 29.10. IOSchedulingClass=idle не помогает: метаданные и коммит btrfs пишет ядро вне cgroup.
Нужно: удалять пачками с допуском по моментальному состоянию (decisions.md instant-not-average): перед пачкой ждать, пока dirty+writeback и давление IO (моментальные) ниже порога; размер пачки и паузы — по замеру.
Отдельно (не чинить, число в итог): какая доля записи на SSD — ~100 ГиБ/сутки в GOCACHE.

## Приёмка
- Замер ночи (или ручной прогон на тех же объёмах): фризов от trim 0 по сторожу фризов, время trim — сколько выйдет.
- Тест: пачка не начинается, пока моментальная мера выше порога (подменный /proc).

## Журнал
- 2026-10-31 10:11 открыт — graft-master
- 2026-10-31 10:11 в работе — graft-master
- 2026-10-31 10:11 note — graft-master: 10:15 боец Luna, бриф /home/aisee/.cache/worktrees/graft-measure/brief-gocache-trim-freeze.md; доля GOCACHE в записи: ~100 ГиБ/сут из ~875 ГиБ/сут nvme0n1 (12 881 ГиБ sectors written за 353 ч аптайма) ≈ 11%
- 2026-10-31 10:37 note — graft-master: 11:1x боец сдал раунд 1 (пачки ≤1000 записей/256 МиБ, Dirty+Writeback<64 МиБ, busy kind gocache-trim, syncfs, --max-wait 30m; синтетика 50k×100 КБ: 1.37 с → 5.20 с, 50 пачек). Ревью раунд 1: свод двойной классификации записи, мера meminfo в loadgate — review-gocache-trim-1.md
- 2026-10-31 10:39 note — graft-master: мёртвый параметр entry(apply=false) отдан glint-master: тикет glint 20261031-1038-MEDIUM-param-always-same-constant, находки в graft пришлёт
