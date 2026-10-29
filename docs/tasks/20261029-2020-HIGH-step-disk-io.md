# stats.jsonl: чтение, запись и пик записи диска у шага и задачи

Статус: в работе
Исполнитель: graft-master

## Вход
- Запрос мастера соседнего инструмента (задание лида 2026-10-29 ~20:14, умный допуск тяжёлых задач по профилю): допуску нужен профиль диска задач graft; шаги graft идут в cgroup сессии агента, отдельно их не измерить.
- Есть: max_rss_mb из rusage (internal/stats/rss_linux.go), tree_mem_mb из опроса дерева раз в 200 мс (internal/stats/watch.go, tree_linux.go).

## Что сделать
- read_mb, write_mb (kind step, суммой на task): rusage шага ru_inblock/ru_oublock × 512 (Linux: свой процесс и дождавшиеся дети); другие ОС — нет поля.
- peak_write_mbs: в том же опросе дерева сумма write_bytes из /proc/<pid>/io; дельта по pid к прошлому опросу (новый pid — целиком); наибольшая запись за скользящее окно ≥ 1 с; шаг короче секунды — без поля; pid, чей io читать нельзя (EACCES, sudo), не считается. На task — наибольший из шагов.
- profile (step и task): `<репо>:<задача>`, репо — папка главного дерева (родитель git common dir), общая у worktree; graft exec — `<репо>:exec:<первое слово команды>`; вне репо — папка запуска. Отдельное поле, project не трогать (уточнение заказчика 2026-10-29).
- README (поля stats.jsonl).

## Приёмка
- Тест stats: шаг, пишущий файл с fsync N МБ — write_mb ≥ N; peak_write_mbs > 0 у шага дольше секунды; Add суммирует read/write, берёт max пика; интеграционный: строки step и task в stats.jsonl с полями. `go run . all`.

## Журнал
- 2026-10-29 20:20 открыт — graft-master
- 2026-10-29 20:20 в работе — graft-master
- 2026-10-29 20:29 пик: не дельты по pid, а сумма write_bytes живого дерева с отсечкой падения: io родителя включает пожатых детей (wait_task_zombie), дельты по pid посчитали бы их дважды; нечитаемый (sudo) процесс досчитается при пожатии родителем — в README. Код, тесты (stats_test.go TestWriteRatePeak/TestUsageSumsDiskIO, stats_io_linux_test.go TestStepDiskIO: 32 МБ → write_mb 32.1, peak 32.1 МБ/с; worktree и exec — профиль), README — готовы; дальше all, коммит, ревью. — graft-master
