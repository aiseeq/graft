# glint: 11 находок в коде резерва и снятия при уходе вызвавшего

Статус: открыт

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
