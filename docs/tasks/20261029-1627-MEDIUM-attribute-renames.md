# approved_release.attribute перепривязывает коммит, в сообщении которого назван не тот ключ

Статус: в работе
Исполнитель: graft-master

## Вход
- Проблема: запрос мастера потребителя 2026-10-29 15:5x: коммит назвал сборную задачу-родителя (всегда In Progress), а его работа — сабтаск в Ready for release; `graft deploy check prod` отказывает, attribute на этот коммит гейт отвергает: «names X already: attribute is for a commit that names no item» (attributionProblems, internal/app/deploy.go). Обход — только --unapproved.
- commitKeys уже подставляет item из attribute вместо ключей сообщения: мешает только проверка.

## Что сделать
- attributionProblems: коммит с ключами — допустим; ошибка, только если attribute ничего не меняет (сообщение называет ровно item).
- Вывод проверки: «attributed to ITEM instead of NAMED: reason» для коммита с ключами, прежний текст — без.
- README attribute; тест в deploy_integration_test.go (TestDeploy…Attribut…, строка ~1148): коммит `feat: PROJ-8 named` с attribute на PROJ-9 проходит как PROJ-9, attribute на PROJ-8 — ошибка.

## Приёмка
- Тест красный на main, зелёный после; go run . all; ответ мастеру потребителя с версией.

## Журнал
- 2026-10-29 16:27 открыт — graft-master
- 2026-10-29 16:27 в работе — graft-master
- 2026-10-29 16:31 note — graft-master: attributionProblems: ошибка только если сообщение называет ровно item; вывод «attributed to X instead of Y»; тест в TestDeploy…Attribut (красный на HEAD: check и deploy gate exit 1; зелёный после); README; go run . all EXIT 0
- 2026-10-29 16:39 note — graft-master: ревью суб-агента b94d691: откат перепривязанного коммита называл X (commitKeys теперь берёт item из attribute того, что откат откатывает — для всех потребителей), «instead of» включал сам item, тест не доказывал «только Y» — исправлено, тест: заметка на PROJ-9, не PROJ-8; откат — строка про PROJ-9 (красный без правки); go run . all EXIT 0
