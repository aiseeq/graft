# modules.latest: свои модули спрашивать мимо прокси (GOPROXY=direct) — прокси отдаёт @latest с задержкой после push

Статус: готово
Исполнитель: graft-master

## Что сделать

## Приёмка
- (решение 16:39 — настройка Go вместо кода) `go env GONOPROXY` на ноуте и pc содержит github.com/aiseeq/*; `go list -m <свой модуль>@latest` идёт мимо прокси (`-x`: git ls-remote к github); README — абзац про GONOPROXY.
- Живой: после push в свой модуль `graft commit` в потребителе поднимает пин сразу.

## Что
- Соседняя сессия 31.10 16:33 CET: push модуля из latest-префиксов в 16:16, `graft commit` пин не поднял, гейт проекта упал на сверке пина с соседней папкой.
- gomods.LatestVersions (internal/gomods/gomods.go:159) — `go list -m -e -json mod@latest` с GOWORK=off и окружением пользователя; GOPROXY=https://proxy.golang.org,direct; модуль не в GOPRIVATE → @latest из прокси, у которого кеш @latest после push до ~30 мин (FAQ proxy.golang.org).
- Замер 16:4x: `GOPROXY=direct go list -m A@latest B@latest` 3.2 с против 2.9 с через прокси — цена одна.


## Журнал
- 2026-10-31 16:38 открыт — graft-master
- 2026-10-31 16:39 note — graft-master: 16:4x решение: не код, а настройка Go — go env -w GONOPROXY=<частные префиксы>,github.com/aiseeq/* на ноуте (было пусто → равно GOPRIVATE); GOPRIVATE и sumdb не тронуты. Проверка: go list -m -json sc2kit@latest → Origin.URL github (direct). Код graft не меняю: тесты modules_integration идут через file:// GOPROXY, принудительный direct их ломает; README — абзац про GONOPROXY. PC: go env пуст, graft commit там не идёт — не трогаю. Живая проверка — @latest graft сразу после следующего push.
- 2026-10-31 16:39 в работе — graft-master
- 2026-10-31 16:48 note — graft-master: PC (по просьбе соседней сессии: приёмки --pc гоняют graft gate там): go env -w GONOPROXY=github.com/aiseeq/* (GOPRIVATE там пуст); go list sc2kit@latest → Origin.URL github.
- 2026-10-31 19:22 note — graft-master: 19:21 живая проверка на релизе v2.69.0: сразу после пуша тега go list -m github.com/aiseeq/graft/v2@latest → v2.69.0. Дальше — независимая проверка.
- 2026-10-31 19:27 note — graft-master: 19:3x независимая проверка: GONOPROXY на ноуте и pc есть, go list @latest = v2.69.0 мимо прокси (-x: git ls-remote github), README верен; пункт приёмки про тест GOPROXY=direct устарел — переписан под решение 16:39. Третий префикс из modules.latest не в GONOPROXY — единственный потребитель заменён replace на папку, влияния нет.
- 2026-10-31 19:28 готово — verifier-opus (session graft-master): суб-агент Opus 31.10 19:3x (бриф scratchpad graft-master caller-gone/verify-brief.md, п.4–7): go env GONOPROXY ноут/pc содержит github.com/aiseeq/*; go list -m github.com/aiseeq/graft/v2@latest → v2.69.0, -x показал git ls-remote к github (мимо прокси); README.md:98-101 верен; glint: 11 findings, debt glint-debt-reserve
