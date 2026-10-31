# Откуда GRAFT_HEAVY=local у гейтов бойцов рабочего проекта

Статус: на проверке
Исполнитель: graft-master

## Вход
- Задание лида (его тикет load-control, итог — git-admin): 30.10 14:00–14:25 гейты трёх бойцов рабочего проекта шли на ноуте при свободном PC; в ~/.cache/graft/stats.jsonl причина «GRAFT_HEAVY=local», CPU за окно 2093 с.
- Лид исключил допуск бойца (`graft exec --heavy --local` только для `sh -c 'echo; sleep 60'`). Кандидаты: env юнита бойца, обёртка `worker pc`, вложенный graft внутри шага, .graft.yaml проекта.

## Что сделать
- Найти источник по stats.jsonl и env процессов. Причина в graft — починить; в чужом коде — хозяину (llmark-master — worker, мастер проекта — его конфиг).

## Приёмка
- Названа цепочка, которая выставляет GRAFT_HEAVY=local, с доказательством (запись stats.jsonl, env процесса, строка кода); починка graft — тестом.

## Журнал
- 2026-10-31 02:39 открыт — graft-master
- 2026-10-31 02:39 в работе — graft-master
- 2026-10-31 03:00 note — graft-master: Источник найден. 1) Приёмка worker (llmark tool/cmd/worker/job.go:1666 runAcceptQueued) запускает команду мастера под 'graft exec --heavy --local'; graft по описанию --local (main.go exportExecEnv, help exec) экспортирует GRAFT_HEAVY=local вложенным graft, и 'graft gate' в accept держится на ноуте. Доказательства: acc*.graft.txt трёх бойцов ('heavy, on this machine: GRAFT_HEAVY=local'), stats.jsonl 1811add33c8b9c2c (1453, старт 13:59:42), acc1/acc2 1562 13:48/14:23, acc1 1528 14:07. 2) Третий боец сам писал --local (8ed297a1f4b9a9d9 и др.) — так велел бриф мастера рабочего проекта (brief.md:8 'тяжёлое — graft exec --heavy --local -- …'). Graft работает как описано; починка — у llmark-master (снять GRAFT_HEAVY в acceptWrap перед командой мастера, внешний --local оставить: remote_heavy проекта иначе увёз бы скрипт вне дерева) и у мастера проекта (бриф без --local). Отправлено им и git-admin.
- 2026-10-31 03:00 на проверке — graft-master — передано хозяевам; правки graft нет
