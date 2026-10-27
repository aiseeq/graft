# A FIFO lock waiter fails: ticket for lock disappeared

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема: peers' `graft exec --heavy` and `graft commit` failed with
  `graft: ticket for lock load.lock disappeared` (28.10 ~00:28 two at once,
  ~00:29); a newcomer once showed up ahead of an older waiter.
- Метод: measured — codex's sandbox (agent workers, `~/.cache/graft`
  writable for the heavy queue) has its own PID namespace: inside, `pid=2`,
  `kill -0 <host pid>` fails. A graft there finds every host ticket's pid
  dead and removes them all (`readQueue`); its own ticket, pid 2, is alive to
  the host forever (kthreadd answers EPERM) once that graft dies unremoved.
- Дешевле: none: a pid says nothing across namespaces.
- Инструмент: internal/lock.
- Цена: an hour.
- Если не делать: heavy tasks fail at random while a worker queues; a killed
  worker's ticket blocks the queue for 24h.

## Что сделать
1. A ticket (and a lock holder note) is alive while its owner holds a shared
   flock on it; a reader that takes it exclusively removes it. No pid check.
2. New ticket suffix `.lticket`, so a graft before the change neither reads
   nor removes them (the lock itself stays shared by both).
3. Test: a ticket whose pid is dead but whose lock is held stays; one whose
   lock is free goes.

## Приёмка
- `go test ./internal/lock`; `go run . all`; install.

## Журнал
- 2026-10-28 00:32 открыт — graft-master
- 2026-10-28 00:35 cause measured: `codex sandbox -- sh -c 'readlink /proc/self/ns/pid; echo $$'` → own ns, pid=2
- 2026-10-28 00:41 fix: tickets and holder notes are live files (internal/lock/live.go): the owner holds a shared flock, a reader removes only what it can take exclusively; `.lticket`/`.lholder`, so old grafts neither read nor remove them; a waiter whose own ticket is missing says so (not only on an empty queue). Unit: TestAcquireFIFODropsDeadTicket (held ticket of "pid 2" stays, unheld goes)
- 2026-10-28 00:42 live probe (scratchpad nsprobe.sh: A holds, B waits, D queued, C in `codex sandbox` workspace-write): ~/bin/graft (old) — tickets 2→1, D exit 1 «ticket for lock load.lock disappeared»; new build — tickets 2→3, D and C ran
- 2026-10-28 00:47 committed b870ada (`go run . all` green before); the ticket text named a private project, removed in the next commit
- 2026-10-28 00:58 independent review (fresh agent) of b870ada: createLive failed on ENOENT when a reader removed the file before it was reopened (holder notes, no guard); on Windows a remove or read fails while another handle has the file open. Fixed: ENOENT retries (TestCreateLiveRetriesRemoved, red without), sharing/lock violations on Windows leave the dead file to the next reader. As designed: old .ticket/.holder files stay, old and new grafts share the flock, not the order
