# modules.latest: свои модули спрашивать мимо прокси (GOPROXY=direct) — прокси отдаёт @latest с задержкой после push

Статус: в работе
Исполнитель: graft-master

## Что сделать

## Приёмка
- Тест: latestVersions получает GOPROXY=direct в env (подмена go, запись env).
- Живой: после push в свой модуль `graft commit` в потребителе поднимает пин сразу.

## Что
- Соседняя сессия 31.10 16:33 CET: push модуля из latest-префиксов в 16:16, `graft commit` пин не поднял, гейт проекта упал на сверке пина с соседней папкой.
- gomods.LatestVersions (internal/gomods/gomods.go:159) — `go list -m -e -json mod@latest` с GOWORK=off и окружением пользователя; GOPROXY=https://proxy.golang.org,direct; модуль не в GOPRIVATE → @latest из прокси, у которого кеш @latest после push до ~30 мин (FAQ proxy.golang.org).
- Замер 16:4x: `GOPROXY=direct go list -m A@latest B@latest` 3.2 с против 2.9 с через прокси — цена одна.


## Журнал
- 2026-10-31 16:38 открыт — graft-master
- 2026-10-31 16:39 note — graft-master: 16:4x решение: не код, а настройка Go — go env -w GONOPROXY=<частные префиксы>,github.com/aiseeq/* на ноуте (было пусто → равно GOPRIVATE); GOPRIVATE и sumdb не тронуты. Проверка: go list -m -json sc2kit@latest → Origin.URL github (direct). Код graft не меняю: тесты modules_integration идут через file:// GOPROXY, принудительный direct их ломает; README — абзац про GONOPROXY. PC: go env пуст, graft commit там не идёт — не трогаю. Живая проверка — @latest graft сразу после следующего push.
- 2026-10-31 16:39 в работе — graft-master
