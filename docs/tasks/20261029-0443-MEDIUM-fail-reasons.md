# Журналы: первые строки причины красного прогона

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Запрос мастера потребителя (2026-10-29): в журналах graft хранить первые строки причины красного прогона — имя упавшего теста и его сообщение, либо ошибку сборки пакета или шага. Сейчас tests.jsonl хранит у fail только имя, время, итог; stats.jsonl — только exit.
- tests.jsonl, упавший тест: 4-е поле (как Reason у skip, internal/stats/tests.go TestResult) — его вывод без рамки (`=== RUN`, `--- FAIL`, `=== PAUSE/CONT`), первые 10 строк, строка ≤ 300 символов, всего ≤ 2000, ANSI вырезан. Сабтест держит свой вывод, родитель — свой.
- tests.jsonl, строка пакета: поле `reason` (omitempty) — у build-fail строки build-output пакета (internal/testreport/gotest.go), у пакета, упавшего без упавшего теста (TestMain, ошибка базы test_db, panic вне теста) — его вывод уровня пакета без строк FAIL/ok. Те же лимиты.
- Отчёты jest/playwright (internal/testreport/reports.go): у fail — сообщение об ошибке из отчёта, те же лимиты.
- stats.jsonl: строки step и task с exit != 0 — поле `reason` (omitempty): последние 10 строк stderr шага (пустой stderr — хвост stdout), те же лимиты; у task — reason упавшего шага (inner task — его reason). Вывод на терминал не меняется (tee в кольцевой буфер). Удалённые шаги (remote_heavy) — то же по их выводу.
- README: формат журналов — новые поля. Одна функция обрезки на все места.

## Приёмка
- accept_fail_reasons_test.go (scratchpad accept/reasons): красный на main, зелёный после; `go run . all`; тесты на jest/playwright и пакетный fail без теста — свои у исполнителя, ревью мастера.

## Журнал
- 2026-10-29 04:43 открыт — graft-master
- 2026-10-29 04:59 в работе — graft-master
- 2026-10-29 04:59 note — graft-master: боец Luna 1029-0459-fail-reasons, worktree ~/.cache/worktrees/graft/fail-reasons, приёмка accept/reasons.sh (красная на main 04:58: все 5 проверок)
- 2026-10-29 05:29 note — graft-master: мастер: Luna сдала рабочую версию, приёмка красная из-за моего файла (go fix: SplitSeq, strings.Builder) — исход «не справился» не её; Terra 1029-0521 снята. Ревью: tee stdout/stderr через MultiWriter снимает TTY и вешает graft на фоновом процессе с унаследованным stderr (тест DoNotWaitForABackgroundChild красный 15 с), деплой тоже под захватом. Доводка — worker continue, правка-по-ревью
- 2026-10-29 05:36 note — luna: luna: причины отказов в tests.jsonl и stats.jsonl, единое ограничение текста (stats.LimitReason), проверки Go/jest/playwright и шагов; доводка по ревью: в терминале без захвата, WaitDelay 2 с у захваченного шага, деплой без захвата
- 2026-10-29 05:36 note — graft-master: ревью мастера: доводка верна; свёл повтор WaitDelay, комментарий measureWithCapture, пример README без противоречия (package reason только без упавшего теста, родитель без вывода сабтеста); приёмка перенесена в fail_reasons_integration_test.go; тесты журналов, exec, install, deploy зелёные
