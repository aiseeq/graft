# graft exec --heavy: молча уходит на PC без TMPDIR и прочих отличий окружения вызывающего

Статус: на проверке
Исполнитель: graft-master

## Вход
- chooks-master 01.11 ~03:25: 02:45–03:20 трое (chooks-master, боец Luna 1101-0305, суб-агент-проверяющий) запускали `TMPDIR=<btrfs> graft exec --heavy -- go test …`; graft печатал `graft: exec: heavy, on pc`, TMPDIR не передан, тест, зависящий от ФС, дал другой исход — мутация «прошла зелёной». Поведение задокументировано (--env, --local в help exec).

## Что сделать
- prior по теме; решить: предупреждение при уходе на PC, если у вызывающего заданы переменные, которые не передаются (TMPDIR и др.), с подсказкой --env/--local; или иное.

## Приёмка
- Тест: exec на фейковом хосте при заданном TMPDIR без --env печатает строку-предупреждение с именем переменной; с --env TMPDIR — нет.

## Журнал
- 2026-11-01 03:34 открыт — graft-master
- 2026-11-01 04:24 в работе — graft-master
- 2026-11-01 04:29 note — graft-master: prior: env на хост — тикет remote-caller-env (30.10): /proc/ppid снят из-за ложных срабатываний, выбран --env NAME (git-admin). Предупреждение по TMPDIR отклонено: TMPDIR задан у всех агентских сессий (/tmp/claude-tmp), на PC за 31.10 ушло 1486 задач — шум на каждой. Сделано: строка, которая отправляет graft exec на хост, говорит 'the caller's variables, TMPDIR too, stay here: --env NAME passes one, --local runs here' (97fc360), README; TestExecHeavyRunsOnRemoteHost красный до правки, зелёный после. Дальше — независимая проверка в пачке перед релизом.
- 2026-11-01 04:29 на проверке — graft-master
