# gate of Go modules checks gofmt

Статус: открыт

## Вход
- glint-master 28.10: a consumer project commit passed the gate with 3 unformatted files (gofmt -l); graft adds go vet (and go fix) to every Go module gate, not gofmt; only projects with their own fmt-check task check it.

## Что сделать
1. Built-in gate step after go vet: every git-listed .go file of the Go modules (testdata/vendor/node_modules and go_vet.exclude skipped) must equal its gofmt form (go/format, CRLF taken as LF); lists the files and how to fix.
2. Config go_fmt: {enabled: true by default, reason required to turn off}.
3. README gate section.

## Приёмка
- Integration test: unformatted file fails the gate with its path; testdata file not checked; go_fmt.enabled false + reason passes; mutation red.

## Журнал
- 2026-10-28 14:29 открыт — graft-master
- 2026-10-28 14:32 note — graft-master: spec gofmt_integration_test.go (sha d20d09fe), красная на main 14:33; бриф scratchpad/gate-gofmt/brief.md; worktree ~/.cache/worktrees/graft/gate-gofmt; боец Luna, приёмка /tmp/claude-tmp/graft-gate-gofmt-accept.sh
- 2026-10-28 14:50 note — graft-master: 14:55 боец сдал (приёмка зелёная); ревью 1: принадлежность файла ближайшему модулю (go.work), ctx; worker continue
- 2026-10-28 15:23 note — graft-master: проверка суб-агентом: 12 мутаций, 11 красные; дефекты: удалённый в дереве файл из индекса валил graft gate, файл без package проходил, конфликт — файл трижды, нет текста ошибки разбора, нет теста на удалённый go.mod. Всё исправлено, мутации красные.
