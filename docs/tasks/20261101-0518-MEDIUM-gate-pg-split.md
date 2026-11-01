# Гейт graft: тесты на PostgreSQL — в test-gate-db, test-gate без test_db и с remote: true (на PC при занятом ноуте)

Статус: ждёт
Ждёт: 2026-11-02 06:00
Исполнитель: graft-master

## Вход
- gate-parallel-tests, решение git-admin 01.11 05:3x (п.2): test-gate с test_db graft оставляет на ноуте («the task uses the test database»), а гейт graft — 5,9 CPU-ч в сутки при load ≥ 16 (31.10). План D (pc-offload-plan).
- Прогон `worker pc --tree /home/aisee/git/graft -- go test -count=1 -short ./...` 01.11 05:4x: всё зелёное, кроме TestRefusedCommandMissing: на PC (bwrap без selinuxfs) RefusedCommand пропускал несуществующую программу — существование проверялось только через getxattr SELinux.

## Что сделать
- proc.RefusedCommand: несуществующая программа — ошибка на любой машине (os.Stat до Refused); тест без SELinux (подмена selinuxEnforce) красный до правки.
- .graft.yaml: test-gate без test_db, с remote: true; test-gate-db (test_db) — пакеты, читающие GRAFT_TEST_PG_DSN: internal/flags, internal/testdb, testdb; smoke включает оба.

## Приёмка
- `go run . smoke` зелёный; тесты PG идут (не SKIP) в test-gate-db; в test-gate они SKIP.
- Прогон -short на PC зелёный целиком.
- После выкатки: гейт graft при занятом ноуте пишет «go test and go vet steps of remote: true tasks on pc»; CPU-ч ноута на гейт graft при load ≥ 16 до/после за сопоставимые сутки (31.10 — 5,9).

## Журнал
- 2026-11-01 05:18 открыт — graft-master
- 2026-11-01 05:18 в работе — graft-master
- 2026-11-01 05:19 note — graft-master: сделано в дереве main: RefusedCommand проверяет существование (refused.go), тест TestRefusedCommandMissingWithoutSELinux — красный без правки (мутация), зелёный после; на PC internal/proc зелёный (worker pc 05:2x). .graft.yaml: test-gate remote: true без test_db; test-gate-db (internal/flags, internal/testdb, testdb) — с БД тесты идут (testdb 6,6 с), без DSN 12 SKIP. Коммит — следующим шагом.
- 2026-11-01 05:37 note — graft-master: Влито 9fef946 (test-gate без test_db + remote: true; test-gate-db: ./internal/flags ./internal/testdb ./testdb). Первые прогоны: 05:20 занятый гейт поделился, test-gate ушёл на pc (упал на TestTrimWaitsForDirty — гонка теста: meminfo перезаписывался на месте; исправлено в том же коммите заменой rename); 05:24 и 05:34 машина не занята — test-gate здесь под допуском гейта (when: busy, так задумано). Стена test-gate на pc ≈ как здесь (корень пакета идёт подряд — gate-parallel-tests). Замер после — за сутки: CPU-с test-gate на ноуте при load ≥ 16 до (31.10) / после, доля прогонов на pc, ожидание допуска на pc.
- 2026-11-01 05:37 ждёт — graft-master — 2026-11-02 06:00
