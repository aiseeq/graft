# Тяжёлое с переменными вызывающего не уходит на хост remote_heavy

Статус: в работе
Исполнитель: graft-master

## Вход
- Мастер соседнего проекта 30.10 04:3x CET: `FOO=bar graft exec --heavy -- go test … -run TestEnvProbe -v` → `graft: exec: heavy, on pc`, тест видит `FOO=""`, host PC. Диагностический тест по env-флагу молча `t.Skip` → `ok`; пути вывода ноутбука на PC нет.
- remoteCmd (internal/tasks/remote.go:324) передаёт хосту только env задачи и GRAFT_HEAVY/threads; worker pc на PC — bwrap --clearenv.
- decisions.md pc-load-control: «на PC переносите то, что легко перенести без проблем».

## Что сделать
- stayHere: переменные, которые вызывающий задал graft (окружение graft против окружения родителя, /proc/<ppid>/environ; без GRAFT_*, GIT_*, PWD, OLDPWD, SHLVL, _) → `heavy, on this machine: the caller set FOO, which <host> would not get`. Не Linux — причина «cannot tell the variables the caller set».
- Тесты фикстур, где graft запускает graft.test с другим env: родитель — оболочка-прослойка.
- README, ответ мастеру-заявителю.

## Приёмка
- Интеграционный тест: `FOO=bar graft exec --heavy -- go test` на занятой машине остаётся здесь и называет FOO; без своих переменных — на хост (TestUserRemote* зелёные).

## Журнал
- 2026-10-30 04:30 открыт — graft-master
- 2026-10-30 04:30 в работе — graft-master
- 2026-10-30 05:03 note — graft-master: Подход /proc/<ppid>/environ (0a7b2ca, callerenv*.go) снят до релиза: проверка 30.10 — ложные срабатывания почти на любом родителе (bash -lc бойцов и codex → PATH; терминал bash -ic → GPG_TTY PATH; go run и git-хук → PATH; env -i + bash -lc, как в unit бойца → ~27 переменных rc) — тяжёлое перестало бы уходить на хост; на macOS/Windows remote_heavy выключался. Вместо этого README говорит, что хост не получает окружение вызывающего, и даёт --local. Живой пример той же беды: GOOS=windows graft exec --heavy -- go vet ушёл на pc и проверил linux. Дальше — выбор подхода (явная передача переменных и т. п.), варианты с ценой — в тикет.
