# stats.jsonl и tests.jsonl держат не меньше двух недель, шапка graft stats называет первую запись

Статус: готово
Исполнитель: graft-master

## Вход
- Задание Сна (обход tests 30.10 02:40 CET, dream-master, итог — git-admin): stats.jsonl.1 охватывает 25.10 13:16–29.10 22:51 (4.4 дня), а MaxSize 20 МБ и одна копия (internal/stats/stats.go:26, rotate :416). `graft stats --all --since 7d` пишет «since 2026-10-23 03:22», хотя записей раньше 25.10 нет. Срез Сна за неделю и сравнение с прошлым окном невозможны.
- Читатели копий вне graft: aguard internal/agentwait/graft.go:18, dream и sleep inventory/heavy.go:239 — читают `.1` и сам файл.

## Что сделать
- Ротация в нумерованные копии: .1 — новее, .N — старше; при ротации сдвиг; копия, где последняя запись (mtime) старше Retention, удаляется. Retention 15 дней: два недельных окна и день запаса. Так же для tests.jsonl.
- Load/LoadTests читают все копии, от старших к младшим.
- Шапка `graft stats` и `--tests`: если журнал начинается позже среза — «since <первая запись> (the log starts there, asked <срез>)».
- README (Resource stats, ротация). Сообщить читателям вне graft (Сон — dream/sleep, aguard), что копий теперь несколько.

## Приёмка
- Unit: rotate при нескольких копиях сдвигает и удаляет старые по mtime; Load читает по порядку. Интеграционный тест шапки. `go run . all`.

## Журнал
- 2026-10-30 02:43 открыт — graft-master
- 2026-10-30 02:43 в работе — graft-master
- 2026-10-30 02:52 сделано: ротация в .1…N со сдвигом и удалением копий старше 15 дней по mtime (stats.go shift/oldCopies/Files), Load и LoadTests читают все копии, шапка stats и --tests называет первую запись (app/stats.go sinceText), README; тесты TestStatsFileRotates (сдвиг, удаление 16-дневной), TestStatsCommand (шапка при --since 30d); all зелёный. Дальше — независимая проверка. — graft-master
- 2026-10-30 03:28 note — graft-master: находки ревью: Files/Load/LoadTests пропускают копии старше --since кроме самой старой (заголовок видит начало лога), Append пишет строку при сбое удаления копии; тесты TestLoadReadsTheCopiesOldestFirst, TestAppendWritesWhenAnOldCopyStays, заголовок --tests в TestGoTestsAreRecordedPerTest; вошло в e5ae71d (первый коммит упал на сборке akit 1.7.0) и 37132fa; all зелёный; отдано на проверку суб-агенту
- 2026-10-30 03:32 note — graft-master: читатели вне graft уведомлены: aguard-master (msg 54100dd1, agentwait/graft.go:14-17), dream-master (msg 1e29695e, sleep inventory/heavy.go:238)
- 2026-10-30 03:46 готово — reviewer-opus (session graft-master): суб-агент (свежий контекст, бриф из тикета) 30.10: код принят — stats.go:455-489 сдвиг копий и удаление по 15 дням, Files :525 копии до --since плюс самая старая, sinceText/earliest app/stats.go:167-184 для stats и --tests; go test ./internal/stats ./internal/loadgate ./internal/userconfig ./internal/tasks exit 0, интеграция TestStatsFileRotates|TestStatsCommand|TestGoTestsAreRecordedPerTest exit 0, GOOS=windows go vet exit 0. Недостававшее уведомление читателей сделано: aguard-master подтвердил чтение всех копий (aguard 2e02257), dream-master уведомлён. go run . all exit 0; релиз v2.56.0, install b5c7388
