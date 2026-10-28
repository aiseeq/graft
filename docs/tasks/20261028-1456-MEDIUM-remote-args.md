# user remote_heavy: судить по фактическим аргументам вместо {args}

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
- graft: задача с {args} (glint test: go test -json -p {threads} -v {args} ./...) никогда не уходит в user remote_heavy, даже без аргументов: userRemoteEligible (internal/tasks/remote.go) отвергает сам плейсхолдер.
- Судить по фактическим аргументам запуска: флаг вывода (-o, -coverprofile…) в args → остаётся здесь, иначе можно.

## Приёмка
- Интеграционный тест: busy-машина, задача go test {args}: без аргументов → on pc, с -coverprofile=c.out → on this machine; мутация краснеет.
- Живой: в glint graft test при when: always → on pc.

## Журнал
- 2026-10-28 14:56 открыт — graft-master
- 2026-10-28 15:23 note — graft-master: сделано: {args} судится по фактическим аргументам; wouldWait — admitted/вложенная heavy не переливаются, с приоритетом только машина; GRAFT_HEAVY в drop тестов; тест exec-пути trimRun. Мутации m1/m2/m3 красные.
- 2026-10-28 15:47 note — graft-master: проверка суб-агентом c3b67a0: утверждения подтверждены; дыры: -test.X флаги, {args} внутри слова, -fuzz/-mod=mod, нет тестов приоритета и Compact. Исправлено + TestGoCheckNoOutput, TestCheckGoFmtListsAConflictOnce, приоритет в HeldQueue; мутации красные. Вложенная heavy (r.heavy>0) без теста: enterHeavy не входит в очередь при r.heavy>0, wouldWait повторяет это условие.
- 2026-10-28 15:54 готово — verify-subagent: суб-агент c3b67a0: 5 утверждений подтверждены мутациями; дыры (-test.X, {args} в слове, -fuzz/-mod=mod, тесты приоритета и Compact) закрыты в 15af99c, мутации красные; живой: glint graft test при when: always → on pc, код 0, 25 с; v2.49.1
