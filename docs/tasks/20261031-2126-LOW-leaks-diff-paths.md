# leaks: путь в находке искажён для имён с пробелом (хвостовой TAB) и с кавычкой (C-строка git)

Статус: готово
Исполнитель: graft-master

## Что сделать
- Находка проверки leaks-merge-commits (a3de706, пробы 13 и edges): ParseDiff берёт путь из строки «+++ b/…» как есть — для пути с пробелом git дописывает TAB (находка «a b.txt\t:1»), путь с кавычкой или управляющим символом git пишет C-строкой в кавычках «+++ "b/…"» — путь в находке пуст. Касается и обычных коммитов, и staged.
- internal/leaks/scan.go ParseDiff, ветки +++ b/ и +++ .

## Приёмка
- `graft leaks` и проверка staged (`graft check`/`graft commit`) называют точный путь для имён с пробелом, кавычкой, `\`, TAB, не-ASCII (мера брифа проверки scratchpad leaks-merge/paths-verify-brief.md, записана до проверки); удалённый и бинарный файл не ломают разбор; битая C-строка — ошибка, не пустой путь.
- Тесты TestParseDiffOddPaths и TestLeaksNamesAnyFileName красные до правки.

## Журнал
- 2026-10-31 21:26 открыт — graft-master
- 2026-10-31 21:27 note — graft-master: 21:2x мастер сам (мелкий баг, 10 мин): worktree leaks-diff-paths, headerPath — снять хвостовой TAB, strconv.Unquote для C-строки git, /dev/null и не b/ → ""; битая кавычка — ошибка. Тест TestParseDiffOddPaths красный до правки (malformed passed, пути), зелёный после.
- 2026-10-31 21:30 note — graft-master: Влито ff de5701d на main. Независимая проверка — свежий суб-агент a9f78e1, бриф scratchpad leaks-merge/paths-verify-brief.md. go run . all на de5701d — перед релизом v2.70.2 (leaks merge + пути).
- 2026-10-31 21:38 note — graft-master: Проверка a9f78e1: мера выполнена (пробел, кавычка, \, TAB, кириллица, LF, \001, \177 — точно в leaks и check; родитель красный). Остаток: имя с байтом вне UTF-8 и кавычкой → U+FFFD (core.quotePath=false оставляет байт сырым, strconv.Unquote его портит). Чиню: core.quotePath=true в трёх вызовах, тест TestLeaksNamesAnyFileName (имя через update-index) красный до правки — «bad\uFFFD\"q.txt».
- 2026-10-31 21:45 готово — verifier-opus (session graft-master): Свежий суб-агент a9f78e1, пробы scratchpad paths-verify/probe.sh: de5701d+8ac2bcb — 15 имён (пробел, кавычка, \, TAB, LF, \001, \177, кириллица, хвостовой пробел, байты вне UTF-8 с кавычкой и без) точные в graft leaks --all и graft check, /dev/null и бинарь не ломают; с глобальным core.quotePath=false — то же; родитель de5701d^ — TAB и пустые пути, de5701d с глобальным false — U+FFFD (контроль).; glint: 12 findings, debt glint-debt-reserve
