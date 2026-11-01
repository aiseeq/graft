# glint: 11 находок в коде резерва и снятия при уходе вызвавшего

Статус: ждёт
Ждёт: установка glint с фиксом closer-delegated-close (glint-master пишет); затем glint check и tk done
Исполнитель: graft-master

## Что сделать

## Приёмка
- GOWORK=off glint check . — 0 medium/low в internal/app/stats.go, internal/loadgate/{loadgate,tree_linux}.go, internal/proc/caller_unix.go, internal/stats/{report,stats}.go.
- recover в опросных горутинах — с логом паники (не молча); резерв при панике снимается.
- go run . all зелёный.

## Что
- glint 31.10 19:3x на main 59eb417: 11 находок medium/low в файлах резерва и снятия при уходе вызвавшего (гейт пропускает: падает на high). Блокируют tk done тикетов latest-direct, exec-caller-gone, queue-adversary.
- Начинать после влития exec-cores (боец 1031-1925 правит loadgate.go).

```
GLINT ANALYSIS RESULTS
============================================================
Files analyzed: 338
internal/app/stats.go
  48: [MEDIUM] Function 'Stats' has cyclomatic complexity 28 (max: 20) (cyclomatic-complexity)
  94: [MEDIUM] range value rec copies stats.Record (440 bytes) on every iteration (large-value-copied-per-element)
internal/loadgate/loadgate.go
  200: [MEDIUM] Function 'WaitWithSettle' has cyclomatic complexity 27 (max: 20) (cyclomatic-complexity)
  434: [MEDIUM] A polling goroutine runs with no recover — a panic in one iteration ends the whole server (background-goroutine-without-recover)
internal/loadgate/tree_linux.go
  58: [LOW] Error from filepath.Glob returned without context; say what failed with fmt.Errorf (error-wrap)
internal/proc/caller_unix.go
  57: [MEDIUM] A polling goroutine runs with no recover — a panic in one iteration ends the whole server (background-goroutine-without-recover)
internal/stats/report.go
  554: [MEDIUM] range value r copies stats.Record (440 bytes) on every iteration (large-value-copied-per-element)
internal/stats/stats.go
  404: [LOW] Error from os.MkdirAll returned without context; say what failed with fmt.Errorf (error-wrap)
  406: [MEDIUM] fl (*flock.Flock) from flock.New is never closed, returned, stored or passed on — what it holds stays open (closer-never-closed)
  434: [LOW] Error from json.Marshal returned without context; say what failed with fmt.Errorf (error-wrap)
  480: [MEDIUM] fl (*flock.Flock) from flock.New is never closed, returned, stored or passed on — what it holds stays open (closer-never-closed)
------------------------------------------------------------
SUMMARY: 11 issues found
```


## Журнал
- 2026-10-31 19:28 открыт — graft-master
- 2026-11-01 02:41 note — graft-master: Сон 01.11 (dream-master, glint на 1363fd01): +1 находка main.go:589 execCmd cyclomatic 23 — в объём тикета. Разбор: closer-never-closed stats.go:406/480 — ложные (gofrs/flock v0.13.1: Unlock → reset → resetFh закрывает fh, Close() = Unlock(); при ошибке Lock — ensureFhState) → glint-master. Остальные 10 настоящие: complexity Stats/WaitWithSettle/execCmd, range-копии stats.Record ×2, recover в опросных горутинах ×2 (с логом, резерв снимается), error-wrap ×3. Исполнитель — боец Luna (класс glint-рефакторинг).
- 2026-11-01 02:41 в работе — graft-master
- 2026-11-01 02:42 note — graft-master: Боец Luna 1101-0242-glint-debt-reserve: бриф /home/aisee/.cache/worktrees/graft-measure/brief-glint-debt-reserve.md, приёмка /home/aisee/.cache/worktrees/graft-measure/accept-glint-debt-reserve.sh (gate + 0 glint кроме flock FP), дерево /home/aisee/.cache/worktrees/graft/glint-debt-reserve. FP flock отправлен glint-master.
- 2026-11-01 03:22 note — graft-master: Боец принят (31 мин). Ревью мастера: тест caller_unix — убран подмен stdout и sleep 10ms (ожидание строки stderr со сроком 10 с); -race -count=5 зелёный; мутация без recover — panic/FAIL. Коммит 62d93fc на glint-debt-reserve (включает akit→latest из гейта). Дальше — независимая проверка суб-агентом.
- 2026-11-01 03:40 note — graft-master: Независимая проверка (суб-агент, 03:20–03:35): код, поведение, glint (2 — только flock FP), all зелёный (2-й прогон; 1-й — флак TestBusyDiskAndSwapHoldTasks/io при нагрузке мутаций), утечек нет; ДЕФЕКТ: TestCallerWatcherPollPanicIsLogged краснеет при stdout в файл/tty — моя правка на ревью убрала подмену stdout, а WatchCaller следит лишь при pipe на обоих. Исправлено: stdout тоже pipe; file-mode и -race -count=3 зелёные. Находка вне объёма: pollCallerGone не повторяет unix.Poll при EINTR — тикет caller-poll-eintr.
- 2026-11-01 03:41 note — graft-master: Повторная проверка (тот же суб-агент) 03:41: принято — file-mode, pty, пакет, -race -count=3, мутация без recover краснеет. Влито ff 0bf7d52 на main. tk done — после установки фикса glint closer-delegated-close (flock FP).
- 2026-11-01 03:41 ждёт — graft-master — установка glint с фиксом closer-delegated-close (glint-master пишет); затем glint check и tk done
