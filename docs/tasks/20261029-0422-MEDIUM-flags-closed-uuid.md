# graft flags: полный UUID закрытого события снова доходит до источника

Статус: готово
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Ревью git-admin к 000c716: show/ack/mute перестали находить закрытое событие по полному id — разбор «почему флаг закрыт» сломан. Полный UUID, которого нет среди открытых, — в Get/Resolve как раньше; префиксы — только по открытым.

## Приёмка
- internal/flags/idprefix_test.go TestIDFullUUIDReachesClosedEvent (красный до правки); прежние тесты префикса зелёные.

## Журнал
- 2026-10-29 04:22 открыт — graft-master
- 2026-10-29 04:22 на проверке — graft-master — ждёт проверки git-admin
- 2026-10-29 04:36 готово — git-admin: git-admin 29.10: graft v2.51.0 на ноуте и PC (~/bin/graft revision a03a1e5, modified=false; 44b9cb6 входит). Живой прогон в рабочем проекте (только show): префикс открытого «ff129f55» — полное событие; «f» — ambiguous со списком двух полных id; «zzz» — «no open event … graft flags status lists them» без запроса к среде; полный UUID закрытого события (ack 23.10, env test) — запрос ушёл к источнику, его ответ «event … not found»; префикс того же закрытого — отказ graft без запроса. Тест TestIDFullUUIDReachesClosedEvent — со слов исполнителя.
