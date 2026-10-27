# A tracker transition to done waits for glint -s low on the deployed sha

Статус: открыт

## Вход
- Проблема: the commit gate fails on high findings only; the rest, low too,
  must be fixed before the ticket is done, and nothing checks it.
- Метод: before a transition to a done status (deploy `transitions`), graft
  runs `glint check . -s low` on the deployed sha; findings keep the item where
  it is, `NOT MOVED … glint: N findings`.
- Дешевле: a check in `tk done` only; does not cover tracker transitions.
- Инструмент: graft deploy transitions, glint.
- Цена: a day.
- Если не делать: low and medium findings pile up past done.

## Что сделать
1. Find the transitions that mean done, the deployed sha, how glint is run.
2. Run glint on that sha (a detached work tree), count findings.
3. Findings → not moved, said in a line; the next deploy or handover moves it.
4. Release acceptance: an exception naming a ticket for all the findings;
   how it is named — in README.

## Приёмка
- Tests: a project with a low finding does not move; without findings moves;
  the release exception moves with its ticket named.

## Журнал
- 2026-10-27 23:57 открыт — graft-master
