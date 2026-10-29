# stats reason шага tests: go — из разобранного прогона, не сырой JSON

Статус: открыт

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Замер мастера потребителя на v2.52.0 (2026-10-29 05:42): у шага задачи с `tests: go` stderr пуст, stats.jsonl `reason` берёт хвост stdout — а это сырой поток `go test -json` (`{"ImportPath":…,"Action":"build-output",…}`): 2000 символов уходят на обёртку, текст причины обрезан.
- Сделать: у шага `tests: go` reason в stats.jsonl (step и task) — из разобранного прогона (internal/testreport/gotest.go): по порядку событий — упавший пакет без упавшего теста или build-fail: `<пакет>: <его reason>`; упавший тест: `<пакет> <тест>: <его reason>`; через stats.LimitReason. Сырой stdout такого шага — никогда не reason; хвост stderr — только если разобранное пусто. jest/playwright — как сейчас (их stdout — вывод человека, JSON идёт в файл отчёта).
- README (stats.jsonl reason): сказать про tests: go.

## Приёмка
- accept_go_step_reason_test.go (scratchpad accept/goreason): красный на main, зелёный после; прежние TestFailReasons*, тесты stats/testreport; `go run . all`.

## Журнал
- 2026-10-29 05:43 открыт — graft-master
