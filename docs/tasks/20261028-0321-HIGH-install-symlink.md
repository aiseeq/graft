# install step keeps a destination symlink to the built file and calls it unchanged

Статус: готово
Исполнитель: graft-master

## Вход
- Problem: when the destination of an install step is a symlink to the built file (an old install scheme), the step reads the bytes through the link, says `unchanged`, and keeps the link. The next build then replaces the installed program, bypassing "only what is committed". Reported by the space admin, 28.10.
- Method: Lstat the destination; a symlink is never unchanged, and the rename replaces the link itself with the file.

## Что сделать

## Приёмка
- install_integration_test: a destination symlink to bin/tool turns into a regular file with the bytes of bin/tool, and the output is not `unchanged`.

## Журнал
- 2026-10-28 03:21 открыт — graft-master
- 2026-10-28 03:21 в работе — graft-master
- 2026-10-28 03:21 note — graft-master: fix: sameFile Lstat's the destination, a symlink is never unchanged; TestInstallStepReplacesSymlink red before ("unchanged"), green after; README
- 2026-10-28 03:25 на проверке — graft-master — v2.45.2 installed; independent check: the reporter re-runs install on a symlinked destination
- 2026-10-28 03:27 готово — git-admin: space admin's check on one project: a destination symlink to bin/x, graft v2.45.2 install printed 'x e6ae0f1 -> ...' without unchanged; ls -l a regular file -rwxr-xr-x, readlink exit 1; go version -m vcs.revision=e6ae0f1, modified=false; the binary runs. glint Low error-wrap in sameFile fixed after
