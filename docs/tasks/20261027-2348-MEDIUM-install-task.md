# One graft task installs a project binary into ~/bin

Статус: открыт

## Вход
- Проблема: projects install their binaries into ~/bin each its own way (an
  `install` task running sh, a script of the project, a Makefile target, none).
- Метод: one form of the task, graft-run, differences only where a project
  needs them.
- Дешевле: a README recipe alone, no graft change; to decide after the census.
- Инструмент: graft tasks (`.graft.yaml`), README.
- Цена: about a day with the census and the handover to project agents.
- Если не делать: every project agent keeps its own install, atomicity and
  rebuild rules differ.

## Что сделать
1. Census of every ~/git project: how it installs, what it installs, extras.
2. One form (a built-in or a documented task shape), in graft and README.
3. Edits to each project's `.graft.yaml` go to that project's agent.

## Приёмка
- Every project that installs a binary does it by the one form; the rest
  named with the reason.

## Журнал
- 2026-10-27 23:48 открыт — graft-master
