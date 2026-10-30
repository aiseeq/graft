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
