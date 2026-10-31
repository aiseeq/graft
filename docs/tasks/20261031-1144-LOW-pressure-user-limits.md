# Пороги занятости из akit pressure.UserLimits: в load: graft только очередь

Статус: готово
Исполнитель: graft-master

## Вход
- akit-master 31.10 ~11:40 (обход Сна 31.10, п.9 design-ревизии; тикет akit 20261031-0300-LOW-busy-quiet-why п.7): пороги машины в двух местах — graft userconfig.DefaultLoad (копии MaxSwapKBs 1024, SwapWindow 1s из pressure.DefaultLimits) + load: в config.yaml graft; busy, aguard, worker берут pressure.DefaultLimits. 29.10→30.10 меры расходились ~20 ч из-за load.max_io_full 100.
- Решение (graft-master «да» 31.10): правило и пороги — akit pressure.UserLimits() (~/.config/akit/pressure.yaml через XDG_CONFIG_HOME; битый файл и неизвестный ключ — ошибка). graft — потребитель.

## Что сделать

## Приёмка
- Тест: ключ max_io_full в load: — ошибка с подсказкой; pressure.yaml с max_swap_kbs меняет допуск graft (подменный /proc).
- `go run . all` зелёный; `busy` и `graft` на одном /proc дают одно решение.

## Что сделать (после выхода akit с UserLimits)
- internal/userconfig: убрать из MachineLoad wait_above, max_io_full, max_mem_some, max_swap_kbs, swap_window; правило — pressure.UserLimits(). Ключи порогов в load: — ошибка конфига с подсказкой про pressure.yaml.
- loadgate: Limits из UserLimits.
- Тестовые фикстуры (load: {… swap_window: 10ms}) — писать pressure.yaml в свою XDG-папку.
- README: раздел load и пороги.


## Журнал
- 2026-10-31 11:44 открыт — graft-master
- 2026-10-31 11:44 ждёт — graft-master — akit с pressure.UserLimits (версию пришлёт akit-master)
- 2026-10-31 12:01 note — graft-master: akit v1.17.0 (58c1c9b) выпущен с pressure.UserLimits/ReadLimits/LimitsPath, LimitKeys, QuietNotePath; ожидание снято — берусь.
- 2026-10-31 12:17 в работе — graft-master
- 2026-10-31 12:17 note — graft-master: боец Luna по брифу /home/aisee/.cache/worktrees/graft-measure/brief-pressure-user-limits.md, дерево fix-pressure-user-limits. Пользовательские конфиги graft на ноуте и pc ключей порогов в load: не имеют — удаление ничего не сломает.
- 2026-10-31 13:13 note — graft-master: e8ce532 в main. Ревью мастера: сторож фикстур ужесточён — число своих записей user config в файле ≤ числа writeTestPressure (мутация: убрал один вызов в load_integration_test.go — красный). Мутация UserLimits→DefaultLimits в loadgate роняет TestPressureUserLimitsControlSwapAdmission и TestBrokenPressureLimitsFailBusyCheck. all 13:06–13:12 зелёный; корневой пакет 120.7 с против 123.5/131.0 в прошлых all — без роста.
- 2026-10-31 13:18 на проверке — graft-master — v2.67.0 (e8ce532) установлен 13:15 на ноутбук и pc. Проверка: pressure.yaml с max_swap_kbs меняет решение graft (TestPressureUserLimitsControlSwapAdmission); load.max_io_full в config.yaml graft — ошибка с путём pressure.yaml (TestLoadPressureLimitsMustMoveToAkit); busy и graft на одном /proc — одно правило akit.
- 2026-10-31 13:20 готово — akit-master (session graft-master): akit-master 31.10 13:17–13:19 CET, graft v2.67.0 и busy на одном pressure.yaml (временный XDG_CONFIG_HOME), 4 случая совпали: runnable 1 — оба ждут, причина «(pressure.yaml runnable)»; runnable 500 — оба свободны; max_io_full 0.01 — оба ждут, причина «(pressure.yaml max_io_full)»; runnable: x — оба ошибка pressure.yaml:1 без отката на умолчания
