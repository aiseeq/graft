# План D п.3: glint check на PC при занятом ноуте, со сверкой версии glint

Статус: ждёт
Ждёт: 2026-11-01 14:00
Исполнитель: graft-master

## Вход
- pc-offload-plan, «ок» лида 01.11 05:1x: glint 10,3 CPU-ч на ноуте 31.10, 6,9 при load ≥ 16; правило user remote_heavy пускает только go test/vet (userRemoteEligible, internal/tasks/remote.go), деление гейта — тоже только go test/vet.

## Что сделать
- `glint check` без флагов, пишущих файл (-o в файл и т. п. — сверить с glint check -h), — допустимый шаг для user remote_heavy и для деления занятого гейта, как go test/vet.
- Перед отправкой сверять версию glint на хосте с ноутом (одна проверка на прогон graft); не совпала — шаг здесь со строкой `graft: <task>: heavy, on this machine: glint on pc is <v1>, here <v2>`.
- README.

## Приёмка
- Тесты на фейковом хосте: glint-шаг уходит; с флагом записи файла — остаётся; другая версия glint на хосте — остаётся со строкой.
- До/после: CPU-ч glint на ноуте при load ≥ 16 по stats за сопоставимые сутки, ожидание допуска на PC не дольше ноута.

## Журнал
- 2026-11-01 05:09 открыт — graft-master
- 2026-11-01 05:12 note — graft-master: сверка версии без обращения к PC: install пишет sha256 скопированного на хост файла в <cache>/graft/hosts.json, перед отправкой glint check сравнивается sha glint по PATH. Боец Luna: бриф /home/aisee/.cache/worktrees/graft-measure/brief-pc-glint-remote.md, дерево /home/aisee/.cache/worktrees/graft/pc-glint-remote, приёмка accept-pc-glint-remote.sh (gate + 4 названных теста).
- 2026-11-01 05:12 в работе — graft-master
- 2026-11-01 05:55 note — graft-master: Боец Luna 1101-0512 принят (4/4, 34 мин, 0,018% недели). Ревью мастера: sha сверялась у glint по PATH, даже если шаг задаёт путь — исправлено мной (glintCopy(argv0), exec.LookPath(argv0)). Коммит 8dddb8b в ветке. Независимое ревью Terra: бриф graft-measure/review-pc-glint-remote.md, ответ → .answer.md.
- 2026-11-01 06:14 note — graft-master: Ревью Terra (повтор, review-pc-glint-remote.answer2.md): правки п.1 и п.6 верны; новый край — относительный путь glint разрешался от каталога процесса — исправлен (d4c3bb1→6f0679a: glint только по имени или абсолютному пути, TestGlintCheckNoOutput, мутация красная). Мутации m1–m6 по тестам приёмки — все красные (мастер). Влито ff в main: 95d88cc, 8aa50b3, 6f0679a. Блокер эффекта: песочница worker pc не видит /home/aisee/bin (glint и graft там нет, PATH без него) — запрос llmark-master 06:2x. Дальше: после правки worker — проба worker pc … glint check, релиз, glint-master: install glint новым graft (запись hosts.json), задачам glint в гейтах — remote: true (рассылка п.1); замер до/после.
- 2026-11-01 06:14 ждёт — graft-master — 2026-11-01 14:00
- 2026-11-01 06:34 note — graft-master: 06:3x: worker pc видит /home/aisee/bin (llmark 12f439b), проба мастера — glint check graft в песочнице pc: No issues found, 361 файл. Релиз v2.71.0 (all зелёный, авторы — noreply, leaks 0), установлен на ноут и pc; hosts.json пишется (graft → pc). graft: задаче glint remote: true (cf10fc4). Ждёт: glint-master — graft install glint (запись копии glint), затем первый занятый гейт с glint на pc и замер до/после.
- 2026-11-01 06:50 note — graft-master: 06:5x: glint-master поставил glint через v2.71.0, hosts.json: /home/aisee/bin/glint 56e05b43… = sha256 на ноуте и pc (сверка мастера). Механизм живой; ждёт первого занятого гейта с glint на pc и замера до/после (14:00).
- 2026-11-01 07:17 note — graft-master: Первый живой: 07:15:11 glint check graft — на pc, 7,3 с (коммит 6853096, занятый гейт).
