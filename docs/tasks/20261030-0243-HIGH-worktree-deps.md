# Свежий graft worktree add проекта с фронтом проходит гейт и jest без ручного npm ci

Статус: в работе
Исполнитель: graft-master

## Вход
- Задание Сна (обход tools 30.10, итог — git-admin; отчёт обхода Сна round-tools.md): в свежем worktree двух закрытых проектов с фронтом (A, B — имена в отчёте обхода) нет frontend/node_modules — гейт (lint-changed: tsc, eslint) и jest стоят; 15 бойцов проекта A 29.10, бойцы сами запускали npm ci мимо очереди, мастер A симлинкует node_modules своим циклом, мастер B ставил npm ci руками.
- graft: worktree.copy — файлы, worktree.setup — задачи (README ~1907); каталога зависимостей принести нечем. A: .graft.yaml:74 `worktree: {copy: [.env], setup: [generate-config]}`; B — без worktree:.

## Что сделать
- Выбрать механизм с мастерами A и B через work-admin (варианты Сна: worktree.link — симлинк на каталог главного дерева при совпадении lock-файла, иначе setup-задача с npm ci под heavy; или только setup-задачи в проектах).
- Реализовать в graft, README, тесты; проекты переводят их мастера.

## Приёмка
- Интеграционный тест: worktree add с lock-файлом, равным главному, получает каталог без установки; при расхождении — setup под heavy. В A: свежий worktree → graft gate проходит без ручных шагов (мастер A).

## Журнал
- 2026-10-30 02:43 открыт — graft-master
- 2026-10-30 02:43 в работе — graft-master
- 2026-10-30 03:36 note — graft-master: work-admin передал план мастерам A и B; A (его мастер) 30.10: годится; 4 каталога на 2 lock-файла (корень npm workspaces + 2 пакета workspace на одном lock, e2e на своём), нужна группа: paths с общим key и одной setup-задачей; задачи установки нет — заведёт по ключу; на симлинках у бойцов 29–30.10 jest, tsc, glint проходили. Схема: link: [{key: [...], paths: [...], setup: task}], одна setup-задача запускается один раз на все группы. Ждём B.
- 2026-10-30 03:43 note — graft-master: B (его мастер) 30.10: годится, проба симлинком на деле (lint, tsc, e2e type-check, next build, playwright --list — exit 0; в главное дерево ничего не записано); линковать только frontend/node_modules; задачу установки заведёт вместе с блоком link после выхода ключа
