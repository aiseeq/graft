# A tracker transition to done waits for glint -s low on the deployed sha

Статус: в работе
Исполнитель: graft-master

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

## План
- Config: target `release_notes.glint: <severity>` (opt-in, graft does not know
  glint otherwise; the rollout to projects goes to their agents).
- `noteMover.move` (internal/app/deploy.go): before the transition, once per
  run, `glint check . -s <sev> -o json` in a tree at the deployed sha (the work
  tree when HEAD is that sha and clean, else a detached `git worktree`), count
  `summary.total`; glint failing or panicking rules → not moved too.
- Findings → `NOT MOVED to X: glint: N findings …`, held like require_comment
  (held file, retryHeld, handover); messages of held items made generic.
- Handover reads the deployed sha (`readSHA`); accepted without require_comment
  when glint is set.
- `--glint-ticket KEY` on `graft deploy <target>` and `handover`: an open item
  of the project's keys takes the findings; the items move, a line says so.
- Out of scope: sweep (moves to Done what the target runs in full, after
  Testing already checked it).

## Приёмка
- Tests: a project with a low finding does not move; without findings moves;
  the release exception moves with its ticket named.

## Журнал
- 2026-10-27 23:57 открыт — graft-master
- 2026-10-28 00:17 план записан (раздел План)
- 2026-10-28 00:17 в работе — graft-master
- 2026-10-28 00:23 code, README, tests (glint_integration_test.go, config cases) done; committing
- 2026-10-28 00:29 review (fresh agent) of 2e9cb71: sweep moves held items to Done without the check; rulesPanicked is under stats; a bad --glint-ticket shows only after the deploy; no deployed sha gives a git error; a held key delivered again but skipped is dropped (older). Fixing all; --glint-ticket not tied to an acceptance: graft cannot tell one, said in README
- 2026-10-28 00:31 fixed: sweep checks glint (TestSweepGlintHoldsItems, red without), stats.rulesPanicked, ticket read before the deploy, empty deployed sha, held keys kept when unread or unposted; README
