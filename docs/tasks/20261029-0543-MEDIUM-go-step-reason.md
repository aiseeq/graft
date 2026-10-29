# stats reason шага tests: go — из разобранного прогона, не сырой JSON

Статус: готово
Исполнитель: luna

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
- 2026-10-29 05:45 в работе — graft-master
- 2026-10-29 05:45 note — graft-master: боец Luna, worktree ~/.cache/worktrees/graft/go-step-reason, приёмка accept/goreason.sh (красная на main 05:45: reason — сырой JSON)
- 2026-10-29 05:54 note — luna: luna: tests: go теперь пишет в stats.jsonl причины из разобранных package/test runs; README обновлён.
- 2026-10-29 05:54 note — graft-master: ревью мастера: верно; Close разборщика гарантирован и когда шаг не стартовал (раньше был внутри обратного вызова reason), комментарии к новым функциям; приёмка перенесена в fail_reasons_integration_test.go; тесты журналов зелёные
- 2026-10-29 05:59 готово — graft-master: ревью мастера по диффу; TestFailReasonsOfAGoTestStepAreParsed и тесты журналов зелёные; гейт коммита 34ded04; glint чист после выноса goTestStep
- 2026-10-29 06:03 note — graft-master: проверка мастера потребителя на v2.52.1: прогон с отсутствующим пакетом — reason step и task читаемый (ошибка сборки пакета), сырого JSON нет
