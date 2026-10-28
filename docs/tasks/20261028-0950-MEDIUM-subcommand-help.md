# command -h prints its help section

Статус: на проверке
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- `-h`/`--help` у любой команды печатает её раздел из `graft help` с rc 0; у exec там `--local` и коды 125/126/127. `graft help <команда>` — по возможности. Ошибочный вызов остаётся ошибкой с usage. (Сон, обход tools 28.10, через лида; итог git-admin)

## Приёмка
- `graft exec -h`, `graft worktree -h`, `--help` у всех команд: rc 0 и раздел; тест на каждую команду.

## Журнал
- 2026-10-28 09:50 открыт — graft-master
- 2026-10-28 09:57 note — graft-master: сделано в дереве: help.go (details, helpCmd, sectionOf, writeSection), main.go (newFlags.Usage → раздел + флаги, stdout; sectionHelp в dispatch для команд без своего flagset), тест TestEveryCommandAnswersHelpWithItsSection зелёный; README строка help. Дальше — graft commit.
- 2026-10-28 10:18 на проверке — graft-master — v2.46.0 (3425212), установлено 4908be6; ждёт независимой проверки
