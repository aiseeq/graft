# graft на akit v1.7.0: kind в pressure.Check, порог диска 9.6 за 5 с, источник диска в пропусках и профиль диска в stats

Статус: в работе
Исполнитель: graft-master

## Вход
- akit-master 30.10 (письмо сессии): akit v1.7.0, тикет akit io-now. `(*Meter).Check(ctx, l, kind, fresh)`; kind — вид задачи, тот же ключ, что profile в stats.jsonl (aguard собирает по нему профили).
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
- 2026-10-30 03:28 note — graft-master: сделано в e5ae71d: kind=profile в Wait/BusyNow, skip «…, PSI total used» с DiskFault, MaxIOFull=pressure.DefaultLimits (9.6), disk_profile в строке задачи, XDG_RUNTIME_DIR тестов изолирован (на машине живой io-state.sock); тест интеграции 37132fa; all зелёный; отдано на проверку суб-агенту
- 2026-10-30 03:32 note — graft-master: по ревью: без PSI текст «and no IO pressure either: the disk is not checked» (тест no PSI either), README «once per look at the machine»; пути в тикетах заменены на ~ и имена
- 2026-10-30 03:39 note — graft-master: aguard-master 30.10: профилей вида <repo>:<task> у aguard нет (задачи graft в cgroup сессии), disk_profile пока пуст; поле необязательное, остаётся
