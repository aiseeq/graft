# Пороги занятости из akit pressure.UserLimits: в load: graft только очередь

Статус: ждёт
Ждёт: akit с pressure.UserLimits (версию пришлёт akit-master)

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
