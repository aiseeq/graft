# command -h prints its help section

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
- `-h`/`--help` у любой команды печатает её раздел из `graft help` с rc 0; у exec там `--local` и коды 125/126/127. `graft help <команда>` — по возможности. Ошибочный вызов остаётся ошибкой с usage. (Сон, обход tools 28.10, через лида; итог git-admin)

## Приёмка
- `graft exec -h`, `graft worktree -h`, `--help` у всех команд: rc 0 и раздел; тест на каждую команду.

## Журнал
- 2026-10-28 09:50 открыт — graft-master
- 2026-10-28 09:57 note — graft-master: сделано в дереве: help.go (details, helpCmd, sectionOf, writeSection), main.go (newFlags.Usage → раздел + флаги, stdout; sectionHelp в dispatch для команд без своего flagset), тест TestEveryCommandAnswersHelpWithItsSection зелёный; README строка help. Дальше — graft commit.
- 2026-10-28 10:18 на проверке — graft-master — v2.46.0 (13b0336), установлено 96efaaa; ждёт независимой проверки
- 2026-10-28 11:09 готово — verify-subagent: независимый суб-агент graft-master 28.10 ~10:30 на ~/bin/graft v2.46.0-1-g4908be6: 25 команд + stop/restart/logs, -h и --help код 0, stdout, начало = graft help X; у exec --local и 125/126/127; подкоманды отвечают разделом; побочных действий нет (git status, HEAD, testdb); ошибочные вызовы прежние (exec 125, прочие 2). Вне требования: hook pre-commit -h запускает хук (как до правки); при сломанном .graft.yaml справка падает (как голый graft help до правки). Правки glint после проверки — 5962c4f, 227befb, без изменения поведения (тест TestEveryCommandAnswersHelpWithItsSection в гейте зелёный).
