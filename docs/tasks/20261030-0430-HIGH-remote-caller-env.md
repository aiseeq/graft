# Тяжёлое с переменными вызывающего не уходит на хост remote_heavy

Статус: ждёт
Ждёт: выбор git-admin из вариантов 1–4 (письмо 06:2x)
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
- 2026-10-30 06:08 note — graft-master: Варианты (06:2x), цена — правка и риск:
1) Как есть: README и --local / GRAFT_HEAVY=local. Цена 0. Риск остаётся: тест по env-флагу на хосте молча t.Skip → ok; GOOS=… go vet проверяет linux.
2) graft exec --heavy --env NAME[,NAME] (повторяемый): хост получает названные переменные со значениями вызывающего через env в remoteCmd. Цена ~40 строк + 2 интеграционных теста; риск — забыть флаг (тот же, что в 1), секрет, названный явно, уходит на хост — это осознанный выбор.
3) remote_heavy.pass_env в user config (список имён или префиксов, напр. GOOS GOARCH GOFLAGS *_PROBE): всегда передаются, если заданы. Цена ~60 строк + схема + тесты; закрывает класс GOOS/GOFLAGS без памяти вызывающего; риск — список размыт, секрет с подходящим именем уходит на PC.
4) Остаться здесь, если задана переменная из списка remote_heavy.local_env (обратный 3): цена как у 3; честнее (ничего не утекает), но тяжёлое с GOOS перестаёт уходить.
5) /proc/<ppid>/environ — снят (журнал 05:03).
Рекомендация: 2 + встроенный короткий список 3 (GOOS GOARCH GOFLAGS CGO_ENABLED GOEXPERIMENT — передаются всегда, это не секреты, и без них результат на хосте неверен). Ждёт выбора git-admin.
- 2026-10-30 06:08 ждёт — graft-master — выбор git-admin из вариантов 1–4 (письмо 06:2x)
