# stats.jsonl: disk_profile всегда пуст, read_mb/write_mb/peak_write_mbs никто не читает

Статус: ждёт
Ждёт: 2026-11-01 13:00

## Вход
- Обход design Сна 31.10, п.4 (тикет stats-design-review): disk_profile пуст в 0 из 26 082 строк (Сон); read_mb/write_mb/peak_write_mbs есть в 14 547 строках, читателей нет (git grep по graft, aguard, akit, dream, llmark).
- Решать вместе с aguard-master после проверки smart-admission 01.11 13:00.

## Что сделать
- Убрать поля или показать их в `graft stats --task` — по итогу smart-admission и разговора с aguard-master.

## Приёмка
- Поле либо читается командой (тест), либо не пишется (тест на запись); README в согласии.

## Журнал
- 2026-10-31 03:06 открыт — graft-master
- 2026-10-31 03:06 ждёт — graft-master — 2026-11-01 13:00
