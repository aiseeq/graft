# Имя рабочего проекта в опубликованной истории: вычистить и закрыть класс

Статус: в работе
Исполнитель: graft-master

## Что сделать
1. Вычистить из истории graft имя рабочего проекта в fdc17aa (docs/tasks/20261030-1158-HIGH-io-admission.md:21) тем же способом, что 29.10: filter-repo --replace-text, force-push веток и тегов, свести местный клон и деревья (decisions.md public-repo-names). Делать после влития или архивации веток бойцов 1031-0303/0305/0306: filter-repo переписывает их ветки под работающими бойцами.
2. Закрыть класс: правило «project in context» в ~/.config/graft/private-terms ловит имя рядом со словами graft и бойцов (exec, gate, worker, гейт, тест, мастер, боец, worktrees/…).

## Приёмка
- `graft leaks --branches --tags` в graft — 0 находок с новым правилом; `git log --all -S'<имя> exec'` пуст; github.com/aiseeq/graft fdc17aa недоступен по sha нового дерева.
- Сухой прогон правила по публичным репо в ~/git: новые находки только настоящие.

## Журнал
- 2026-10-31 03:43 открыт — graft-master
- 2026-10-31 03:43 в работе — graft-master
- 2026-10-31 03:43 note — graft-master: Правило установлено 03:42 CET: к строке 7 private-terms добавлены worktrees/<имя>, <имя>:(exec|gate|test|jest|lint), <имя>[ ,]+(exec|gate|deploy|commit|worker|master|e2e|jest|stats), <имя>[ ,]+(гейт|тест|мастер|боец|бойц|приёмк|деплой|коммит), (гейт|тест|мастер|боец|бойц|проект)… <имя>, (exec|gate|worker|master|project|graft) <имя>. Сухой прогон по 8 публичным репо с клоном в ~/git: старое/новое — chooks 56/56, graft 0/1 (fdc17aa, настоящая), glint 759/759, aiarena-web 185/185, akit, talks, plasma-thinkpad-fan, sc2kit 0/0; 13 публичных репо без клона. Синтетика: 12/12 положительных, 0/6 отрицательных (pattern, notice, team). Прежний файл: scratchpad leaks-dry/private-terms.before. Чистка истории — после веток бойцов.
- 2026-10-31 10:32 note — graft-master: 10:30 все ветки бойцов на origin влиты и удалены (на origin только main); жду бойца fix/gocache-trim-batches, затем filter-repo
- 2026-10-31 11:05 note — graft-master: 11:05 CET filter-repo --replace-text ('<имя> exec 630 МБ' → 'exec рабочего проекта 630 МБ') на свежем mirror; бэкап старой истории — mirror scratchpad leak2/backup.git. force-push: main e8cd939→9ff3380 (--force-with-lease), теги v2.60.0–v2.65.0 (деревья v2.63.0+ и main побайтно прежние; v2.60.0–v2.62.0 изменены — они в proxy.golang.org со старым содержимым, убрать нельзя). Местный клон: reset на origin/main после сверки дерева, gc — fdc17aa в клоне нет. Проверка: свежий клон, graft leaks --branches --tags — 0, git log --all -S'<имя> exec' — 0. GitHub по прямому sha fdc17aa ещё отдаёт (висячий, с refs не достижим: compare = diverged). Ссылки на старые хеши (15 коммитов) в 9 тикетах переписаны по commit-map.
