# трейлер Worker: файлы, которые пишет сам graft, — не перенос работы бойца

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема:
- Метод:
- Дешевле:
- Инструмент:
- Цена:
- Если не делать:

## Что сделать
- Отчёт мастера потребителя 2026-10-29: `graft commit` в одном worktree поставил трейлер `Worker:` задания бойца из другого worktree: «the commit stages 3 files as the finished job … left them in <другое дерево>»; эти 3 файла — файл версии, go.mod, go.sum. Их пишет сам graft (бамп версии, `modules.latest`) одинаково в обоих деревьях от общей базы: совпадение содержимого — не перенос работы бойца.
- Причина: internal/workers/workers.go carried() считает доказательством переноса любой staged-файл, совпадающий с деревом бойца и отличный от точки ответвления.
- Сделать: carried() не засчитывает файлы, которые graft пишет при коммите сам: файлы версии проекта (`cfg.VersionFiles()`, internal/config/cache.go) и файлы с базовым именем go.mod, go.sum, go.work, go.work.sum. Список передать в workers.Models из internal/app (withWorkers в hook.go:131; вызовы commit.go:201, hook.go:105, other.go:73 — cfg у вызывающих есть из a.open()). Остальная логика переноса (файлы работы бойца) — без изменений.
- README: в описании трейлера Worker сказать, что файлы версии и go.mod/go.sum/go.work не считаются переносом.

## Приёмка
- accept_worker_own_files_test.go (scratchpad accept/ownfiles): красный на main (трейлер vendor/model-a от VERSION + sub/go.mod + sub/go.sum), зелёный после; TestWorkerTrailers* и тесты internal/workers зелёные; `go run . all`.

## Журнал
- 2026-10-29 10:47 открыт — graft-master
- 2026-10-29 10:49 в работе — graft-master
- 2026-10-29 10:49 note — graft-master: боец Luna 1029-1049-worker-own-files, worktree ~/.cache/worktrees/graft/worker-own-files, приёмка accept/ownfiles.sh (красная на main 10:4x: трейлер vendor/model-a)
- 2026-10-29 10:57 note — luna: luna: Исключил файлы версии и Go module/workspace-файлы из доказательства переноса; приёмка и graft gate прошли.
- 2026-10-29 10:57 note — graft-master: ревью мастера: верно; список go-файлов вынесен в goModuleFiles, path.Base вместо filepath.Base (пути git — через /), переменная path в forkBlobs переименована (затеняла пакет); приёмка перенесена в workers_integration_test.go (TestWorkerTrailersNotFromFilesGraftWrites); glint чист
