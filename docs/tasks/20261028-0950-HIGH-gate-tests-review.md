# gate near 60s limit; test content review

Статус: открыт

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Гейт graft (smoke/test-gate) медиана 55–61 с у предела limits.gate 60 с: профиль корневого пакета (git, подпроцессы, таймауты; TestCommitLockScope/worktree 12 с), кратное ускорение без t.Parallel ради скорости. Замеры graft exec --heavy, было → стало.
- Разбор по содержанию по скиллу test-optimization (образец — разбор тестов по содержанию в рабочем проекте, ключ в письме Сна): стадии, что поймали за месяц, пустые и дубли, ловится ли откат фиксов месяца. Фоном: TestParseTestDBSocket 24.10 красный в 6 worktree, TestGoFixFailsTheGateWithItsDiff флейк 1/46 26.10. (Сон, обход tests 28.10, через лида; итог git-admin)

## Приёмка
- graft stats gate: медиана заметно ниже 60 с на спокойной машине; отчёт разбора в тикете; независимая проверка.

## Журнал
- 2026-10-28 09:50 открыт — graft-master
- 2026-10-28 10:23 note — graft-master: замер 10:35: TestCommitLockScope целиком 0.65 с (graft exec --heavy, -count=1), против 12.1 с у worktree в stats Сна — то число снято под нагрузкой (тесты гоняют поддельный /proc и settle 10ms, своей очереди не ждут). Профиль гейта — только на спокойной машине (busy rc 0), сейчас занята.
- 2026-10-28 12:05 note — graft-master: база 28.10 ~11:40 (машина занята, busy rc 1 по IO): корневой пакет go test -count=1 -json . — 70.9 с wall, CPU 59.0 с (user 25.6, sys 33.4); sys больше user — время уходит на порождение процессов (git/graft на тест). 229 тестов, лидера нет: хвост по ~1 с. stats 7d test-gate: медиана 53.5 с, макс 3m17s, 4% упавших. Лог scratchpad gate-tests/base.json.
- 2026-10-28 12:31 note — graft-master: Разбор exec-t.trace+t2j (strace, wall 168с под strace): git 18862, go 151 (в 36 тестах, 28с=17% wall), graft-процессов ~1300. Топ git-подкоманд: rev-parse toplevel 1336 (раз на процесс), rev-parse --verify HEAD^{commit} 1208 (tasks.go:1454), write-tree 1015, status 900, add -A 813, symbolic-ref 774, cat-file 681, remote 654, rev-parse HEAD 542, --show-object-format 416, config hooksPath 415, --git-path index 390, rev-parse --short HEAD 368. Дороже всех deploy/release-тесты: 300–570 git на тест, ~3с; история строится graft commit (360 коммитов за прогон). План: (a) слить повторные rev-parse/symbolic-ref в один вызов на процесс (Open) и кэш HEAD — мера: git-execs на один graft commit до/после, гейт зелёный; (b) тесты с тулчейном go → test-race/all, как docker; (c) TestExecSettle короче (2,8с → <1с). Числа было→стало — CPU корневого пакета на спокойной машине.
- 2026-10-28 12:32 note — graft-master: Замер (a): сценарий 3 commit+amend+gate+status = 98 git (scratchpad/gitcount/count.sh); повторы rev-parse ~15% — мелко, отложено. Решение: (b) 8 тестов тулчейна go с модулями (modules.latest ×5, ToolsBuildInfo, ToolsUnpinnedFollowLatest, CacheCommandRefusedInASandbox; 10,3с из 73 по calm.json) пропускаются под -short, test-gate идёт с -short, test-race в all — без (защита перед install/release та же, правило test-optimization «тяжёлое — стадия перед release»). (c) TestExecSettle 2s→600ms. Замер было→стало — после проверяющего (машина занята).
- 2026-10-28 12:51 note — graft-master: (b)+(c) сделано: skipGoToolchainInShort в 8 тестах, test-gate с -short, test-race в all без. Замер (graft exec --heavy, тот же бинарь root2.test, test2json): полный 66,6с wall / 59,2 CPU (23,1u+36,1s), -short 51,6 wall / 47,9 CPU (17,6u+30,3s): −15с wall, −19% CPU; 228 pass + 12 skip против 236+4. Settle 2,8→1,3с. Защита: те же 8 тестов в test-race (all перед install/release). Стейл: CLAUDE.md «test-gate — тесты без 4 docker-тестов» — правка инструкций не моя (Сон/пользователь), сообщить git-admin.
- 2026-10-28 12:59 note — graft-master: Гейт после 08a68be: test-gate ~52 с, «over the limit» в коммитах 5c84a9a/08a68be не печатался. Флейки: TestParseTestDBSocket — причина (длинный TMPDIR, длина сокета) уже снята a596364 24.10 23:10; сейчас 200/200 ok. TestGoFixFailsTheGateWithItsDiff — 1 падение 26.10, вывода в stats нет (stats хранит только pass/fail тестов), сейчас 15/15 ok; причину не установить — пробел: graft stats не хранит вывод упавшего теста (кандидат-фича). Осталось: разбор по содержанию (стадии, что поймали за месяц, пустые/дубли, откат фиксов месяца).
- 2026-10-28 13:01 note — graft-master: 13:00: проверка откатом 67 fix-коммитов месяца: scratchpad/revert-check/run.sh (git archive коммита, git apply -R не-тестовой части, go test новых/изменённых тестов через graft exec --heavy) → month.tsv; проба 5c84a9a, 5b4c18c — red-caught.
