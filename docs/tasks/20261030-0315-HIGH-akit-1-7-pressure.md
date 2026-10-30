# graft на akit v1.7.0: kind в pressure.Check, порог диска 9.6 за 5 с, источник диска в пропусках и профиль диска в stats

Статус: в работе
Исполнитель: graft-master

## Вход
- akit-master 30.10 (письмо сессии): akit v1.7.0, тикет /home/aisee/git/akit/docs/tasks/20261030-0230-HIGH-io-now.md. `(*Meter).Check(ctx, l, kind, fresh)`; kind — вид задачи, тот же ключ, что profile в stats.jsonl (aguard собирает по нему профили).
- Мера IO моментальная: фриз сейчас или ≥ MaxIOFull % за 5 с; DefaultLimits.MaxIOFull 9.6; ≥100 выключает дисковую меру.
- Новые сентинелы в Verdict.Skipped: ErrNoIOSource, ErrIOSourceFailed; причина — Load.DiskFault. Диск при этом проверяется по PSI total.
- Load.Disk.Profile (json.RawMessage) — писать в журнал допуска вместе с kind.
- modules.latest уже поднял go.mod до v1.7.0: loadgate не собирается (internal/loadgate/loadgate.go:262).

## Что сделать
- loadgate.Wait/BusyNow принимают kind; tasks передаёт profile задачи (r.profile).
- skip: для ErrNoIOSource/ErrIOSourceFailed — «no disk state source, PSI total used» / «disk state source failed (<DiskFault>), PSI total used», не «not checked».
- userconfig.DefaultLoad.MaxIOFull 9.6, комментарий и README: «% of the last 5 s», 100 выключает.
- Профиль диска последнего замера допуска — полем disk_profile строки задачи в stats.jsonl.

## Приёмка
- go run . all зелёный; тест loadgate: kind доходит до источника диска (фейковый сокет), skip-текст для обоих сентинелов; тест stats: disk_profile в строке задачи при источнике.

## Журнал
- 2026-10-30 03:15 открыт — graft-master
- 2026-10-30 03:15 в работе — graft-master
