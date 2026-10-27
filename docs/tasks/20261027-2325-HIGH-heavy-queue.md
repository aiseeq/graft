# The heavy queue admits one at a time, by the machine of this moment

Статус: в работе
Исполнитель: graft-master

## Постановка
Stats of a shared laptop over 82 minutes: 176 of 179 heavy starts that did
not wait came less than a minute after another one; 27 of 112 gates started
at a load of 22 or more, the load reaching 52. Causes:

- a task finding the 1-minute load average low bypassed the queue
  (`internal/loadgate/loadgate.go` Wait), and every task of a burst found the
  same low figure; a task that waited settled before its own start, not after;
- the gate (`internal/tasks/tasks.go` Gate) did not queue;
- one Go build took every thread (the go command gives a compile up to
  GOMAXPROCS threads, GOMAXPROCS compiles at once), and a task's
  `env: {GOFLAGS: ...}` dropped the user's `-p`.

## Что сделать
1. Every heavy task queues under the FIFO lock, on a calm machine too; it
   keeps the lock `settle` after its start, or until it ends. `free_below` goes
   (strict user config: a config with it fails to parse; minor bump).
2. The gate queues as a whole; heavy tasks inside it, and a graft started by
   an admitted task's steps (`GRAFT_ADMITTED`), are admitted with it.
3. Busy is the rule of `github.com/aiseeq/akit/pressure`: tasks ready to run,
   read once, at or above `wait_above` (default: the machine's threads); IO
   pressure; memory pressure above `max_mem_some` while swapping above
   `max_swap_kbs`. Settle 30s → 10s. Test fakes: a /proc directory, `GRAFT_PROC`.
4. Steps of a heavy task, of the gate and with `{threads}` get `GOMAXPROCS` =
   `load.threads` unless the task's env or the environment sets it; on
   `remote_heavy` too.
5. A task's `GOFLAGS` comes after the user's (environment, else the go env
   file).

## Чем проверить
- `go test ./internal/loadgate`: TestCalmMachineStartsOneAtATime,
  TestBusyDiskAndSwapHoldTasks, TestPriorityBudget.
- `go test -run 'TestGateQueuesForTheMachine|TestHeavyTaskWaitsForLoad|TestTaskGoFlagsKeepTheUsers|TestHeavyTaskRunsOnRemoteHost' .`
- `go run . all` green, then `go run . install`.
- Live, after the install: heavy starts in `~/.cache/graft/stats.jsonl` and
  the gaps between them over the hour after against the hour before — starts
  within 10s of another fall from ~73% to near 0 (priority and nested tasks
  aside).

## Журнал
- 2026-10-27 23:05 akit/pressure private, graft public: asked for a public module; akit opened as v1.0.0 23:16
- 2026-10-27 23:25 открыт — graft-master
- 2026-10-27 23:30 independent review (fresh agent): free_below breaks old configs (accepted, minor); ${GOFLAGS} lost the go env file's flags (fixed: always merged); double fresh measure on priority (fixed in admit); GOMAXPROCS of the environment overridden (fixed); comments
- 2026-10-27 23:47 `go run . all` green
- 2026-10-27 23:50 akit v1.2.0 memory rule adopted, `load.max_mem_some`
- 2026-10-27 23:48 в работе — graft-master
- 2026-10-27 23:58 `go run . all` green on akit v1.2.0 (23:49–23:58); committing --minor
