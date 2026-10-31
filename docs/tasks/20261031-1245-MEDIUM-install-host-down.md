# install: недоступный хост обрывает установку остальных файлов здесь

Статус: готово
Исполнитель: graft-master

## Вход
- akit-master 31.10 12:38: graft install в akit (10 файлов в bin/) при PC без имени (ssh: Could not resolve hostname) поставил здесь только первый файл, остальные 9 остались старыми; поставил их руками.
- internal/tasks/tasks.go install: цикл по файлам — writeInstall, затем rsync на хосты; первая ошибка хоста — return.

## Что сделать
- Сначала все файлы здесь, потом хосты. Хост, на котором rsync упал, на остальных файлах не пробовать. Итог — ошибка (код ≠ 0), где названы хост, причина и файлы, не дошедшие до него; остальные хосты получают свои копии.

## Приёмка
- Тест: два файла, хост down — оба установлены здесь, rsync на down-хост вызван один раз, ошибка называет хост и оба файла; красный на коде до правки.
- `go run . all` зелёный.

## Журнал
- 2026-10-31 12:45 открыт — graft-master
- 2026-10-31 12:45 в работе — graft-master
- 2026-10-31 12:46 note — graft-master: Тест TestInstallHereDoesNotStopAtAHostDown красный на старом коде (two.sh не поставлен, ошибка без его имени), зелёный после правки; все Install-тесты зелёные. README абзац install.hosts.
- 2026-10-31 13:01 на проверке — graft-master — 9c8cf7c в v2.66.1 (установлен 12:58 на ноут и pc); all зелёный 12:50–12:57. Проверка: go test -count=1 -run TestInstallHereDoesNotStopAtAHostDown . и откат правки в internal/tasks/tasks.go install — тест красный.
- 2026-10-31 13:06 готово — akit-master (session graft-master): akit-master 31.10 13:03 CET, graft v2.66.1, graft install в akit при install.hosts: [nohost-akit-check]: код 1, ошибка называет хост и 10 из 10 файлов, все 10 на ноутбуке с mtime 13:03:09
