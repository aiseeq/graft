# Шаги graft без PWD вызывающего: ключ кэша go test не зависит от того, откуда позвали

Статус: готово
Исполнитель: graft-master

## Что сделать
go кладёт в ключ входов теста значения getenv, прочитанные тестом, беря их из окружения самого процесса go (cmd/go/internal/test/test.go, computeTestInputsID → hashGetenv). Тесты читают PWD через os.Getwd, поэтому ключ = PWD, унаследованный graft от вызывающей оболочки: тот же код, позванный из другой папки (`-C`, гейт commit, боец из корня дерева), промахивается мимо кэша (проба хозяина потребителя 31.10 04:2x: `env PWD=<корень> graft -C <корень> test` после прогона из подпапки — два пакета 40 с и 64 с заново).
Решение: BaseEnv (internal/tasks/tasks.go) убирает PWD из окружения шагов. Не ставить PWD=каталог шага: тогда кэш соседнего worktree того же кода перестанет браться. Тесту go сам ставит PWD = каталог пакета (base.AppendPWD), sh ставит PWD сам.

## Приёмка
- Тест: шаг, запущенный graft с PWD=<чужая папка> в окружении, не видит PWD; `env` задачи с PWD по-прежнему его задаёт.
- `graft gate` зелёный.

## Журнал
- 2026-10-31 04:33 открыт — graft-master
- 2026-10-31 04:33 в работе — graft-master
- 2026-10-31 04:34 note — graft-master: 04:40 сам (правка в строку): BaseEnv без PWD, тест TestStepsDoNotInheritTheCallersPWD красный до правки (PWD=/tmp/…/002), зелёный после; README
- 2026-10-31 04:49 note — graft-master: в v2.63.0 (18e3fb0)
- 2026-10-31 04:49 на проверке — graft-master — приёмка: go test -count=1 -run TestStepsDoNotInheritTheCallersPWD .
- 2026-10-31 04:54 готово — llmark-master (session graft-master): llmark-master 31.10 ~04:55, v2.63.0: graft -C <корень llmark> test, затем env PWD=<корень> graft -C <корень> test — все пакеты (cached), в т.ч. cmd/worker и cmd/farm; до правки та же проба 40 и 64 с (письмо llmark-master)
