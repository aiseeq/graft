# graft leaks принимает --all и несколько ревизий

Статус: готово
Исполнитель: luna

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- `graft leaks --all` отвергается разбором флагов («graft leaks <revision range>»), а скан всей истории — это ровно все ветки и теги. Передавать в rev-list все аргументы после `leaks` (включая `--all`, `A ^B`), не одну строку.
- Найдено при чистке истории публичных репо (тикет public-history-work-names): обходил по ref `main..ref`.

## Приёмка
- Тест: `graft leaks --all` в репо с находкой только в теге вне main — находка выведена; `graft leaks main` прежний.

## Журнал
- 2026-10-29 02:41 открыт — graft-master
- 2026-10-29 02:44 в работе — graft-master
- 2026-10-29 02:44 note — graft-master: боец Luna 1029-0244-leaks-all-refs, worktree ~/.cache/worktrees/graft/leaks-all-refs; приёмка — accept/leaks.sh: accept_leaks_all_test.go (красный на main) + graft gate
- 2026-10-29 03:29 note — graft-master: влито 44d276f (main), боец Luna 1029-0244 принят
- 2026-10-29 03:43 готово — graft-master: мастер: ревью диффа, приёмка мастера (TestLeaksTakesAnyRevisions, красный на main до правки) зелёная, на свежих клонах glint и chooks --branches --tags и --glob='refs/pull/*' дали ожидаемые находки
