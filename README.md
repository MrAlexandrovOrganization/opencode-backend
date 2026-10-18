# opencode-backend

## Версии сборки

Go берётся из `go.mod`, версия Alpine — из `versions.mk`. `make versions`
показывает значения. Docker получает их через Make → Compose → build args;
используйте `make compose-build` и `make up`. CI берёт Go из `go.mod`.

Единая точка взаимодействия с [opencode](https://opencode.ai) — HTTP API +
WebSocket шлюз, который управляет сессиями, стримит прогресс, обрабатывает
permissions и вопросы агента. Задуман как **backend-шлюз** для нескольких
фронтендов (Telegram-бот, web, приложение); подробнее — в планировании
`opencode-bot/agents/planning/backend-gateway.md`.

## Возможности

- Пер-пользовательские мультисессионные сессии opencode (создание, переименование,
  удаление, форк).
- Асинхронная отправка сообщений: `POST message` → `202` + локальный `messageID`,
  прогресс приходит через WebSocket-события.
- Нормализация SSE-событий opencode в единый протокол (`message.part.updated`,
  `permission.asked`, `question.asked`, …).
- Permissions (ask/allow/deny) и вопросы агента с серией ответов.
- Загрузка файлов в рабочую директорию, видимую opencode-серверу.
- Непрозрачные токены: в хранилище только SHA-256 хэш.
- In-memory хранилище (интерфейс `store.Store` позволяет подставить SQLite).

## Запуск

Локально (Go 1.26.7):

```sh
go run ./cmd/backend
```

В docker (требует сеть `whisper-net` с работающим `opencode-server` из
`opencode-bot`):

```sh
cp .env.example .env   # задать ADMIN_TOKEN, OPENCODE_PASSWORD
make up
```

Контейнер слушает `127.0.0.1:8090`.

## Конфигурация (env)

| Переменная | Назначение | По умолчанию |
|---|---|---|
| `PORT` | порт HTTP-сервера | `8080` |
| `OPENCODE_BASE_URL` | URL opencode-сервера | `http://localhost:4096` |
| `OPENCODE_USERNAME` / `OPENCODE_PASSWORD` | Basic-auth к серверу | `opencode` / пусто |
| `ADMIN_TOKEN` | токен администратора (генерируется, если пусто) | — |
| `DB_PATH` | путь к SQLite (пусто = in-memory) | — |
| `WORKSPACE_DIR` | каталог для загруженных файлов | `/workspace` |
| `OPENCODE_REQUEST_TIMEOUT` | таймаут одного запроса | `30m` |
| `PERMISSION_MODE` | `ask` / `allow` / `deny` | `ask` |
| `OPENCODE_AGENT` / `OPENCODE_MODEL` | агент и модель по умолчанию | `build` / пусто |
| `OPENCODE_MODEL_FALLBACK` | запасные модели через запятую (`provider/model`), перебираются при недоступности основной; в конце — дефолт opencode | пусто |
| `LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |

## API

- `GET /healthz`, `GET /readyz` — публичные проверки.
- `GET /api/v1/me` — текущий пользователь.
- `POST /api/v1/auth/tokens` — создать токен.
- `GET|POST /api/v1/sessions`, `GET|PATCH|DELETE /api/v1/sessions/{id}`,
  `POST /api/v1/sessions/{id}/fork` — управление сессиями.
- `GET /api/v1/sessions/activity` — единый снимок activity всех доступных
  пользователю сессий: `idle`, `running`, ожидание permission или вопроса.
- `GET /api/v1/sessions/{id}/activity` — живой статус одной сессии (занята ли,
  текущий инструмент/статус, накопленный текст, ожидающие разрешения и вопросы,
  статус opencode-сервера `idle`/`busy`/`retry`).
- `POST /api/v1/sessions/{id}/messages` (202 + messageID), `GET …/messages`,
  `GET …/messages/{mid}` — сообщения.
- `POST /api/v1/sessions/{id}/abort` — прервать запрос.
- `GET /api/v1/commands`, `POST /api/v1/sessions/{id}/commands` — список и
  асинхронный запуск slash-команд OpenCode в сессии.
- `POST /api/v1/sessions/{id}/permissions/{pid}` — ответ на разрешение.
- `POST /api/v1/questions/{qid}` — ответ на вопрос.
- `POST /api/v1/files` — загрузка файла (multipart, поле `file`).
- `GET /api/v1/agents`, `GET /api/v1/providers` — справочные данные.
- `GET /api/v1/ws?session={id|*}` — WebSocket live-событий.

Авторизация: заголовок `Authorization: Bearer <token>` или query `?token=` (для WS).

## Структура

```
cmd/backend/main.go       — запуск, конфиг, сид администратора
internal/api/             — REST-маршруты, middleware, загрузка файлов
internal/engine/          — движок: сессии, async-запросы, SSE → события
internal/opencode/        — HTTP-клиент к opencode-серверу
internal/store/           — интерфейс хранилища + in-memory реализация
internal/ws/              — WebSocket-хаб (подписки, fan-out)
internal/token/           — генерация и хэширование токенов
internal/config/          — конфигурация из env
```

## Команды

`make build` / `make up` / `make down` / `make logs` / `make restart` /
`make deploy` / `make test` / `make vet` / `make format` / `make clean`

## Доставка образов из CI

CI выполняет существующие проверки, затем `make image-build` собирает все собственные
образы из build-секций Compose без runtime-секретов и соседних checkout.
Push main публикует **эти же** образы в GHCR через `make image-publish`.
Тег содержит SHA, run ID и attempt; artifact `image-release` содержит digest manifest.
Платформа — `linux/amd64` (текущая VM). На PR публикации и доставки нет.
Версии build args по-прежнему приходят из Make, определения сборки — из Compose.

SSH CD передаёт SHA, manifest и временный token в `make ci-deploy`.
Эта цель из `makefiles/ci-deploy.mk` под одной блокировкой проверяет чистый main,
отклоняет устаревший workflow, fast-forward обновляет checkout до проверенного SHA,
временно авторизует Docker в GHCR и вызывает `make image-deploy` из нового Makefile.
Сначала скачиваются все образы, затем `image-activate` сохраняет подготовку/миграции
этого проекта и запускает Compose с `--no-build --pull never --wait`.
Доступ к registry удаляется и при ошибке. `ci-deploy` требует переменные
`DEPLOY_SHA`, `IMAGE_MANIFEST_B64`, `GHCR_USER`, `GHCR_TOKEN` из окружения;
не передавайте token аргументом Make или команды.

Короткий SSH-шаг делает fetch и загружает только `makefiles/ci-deploy.mk` через
`git show` из SHA этого workflow. Поэтому первый деплой работает и со старым
checkout без новой цели. Загруженный файл сам проверяет актуальность SHA и повторно
делает fetch после захвата блокировки; параллельные доставки не обновляют checkout
во время активации. Обычные `make up` и существующие `make deploy` сохраняют
свои прежние контракты; для CI служит отдельная цель `ci-deploy`.

Для CI разрешите GitHub Actions запись Packages. Новые GHCR packages по умолчанию
private; настройте наследование доступа от этого репозитория. Автоматический CD
использует временный `GITHUB_TOKEN` с `packages: read` и временный Docker config,
который удаляется после SSH-шагa. Для ручной доставки нужен отдельный read-only
доступ (`read:packages`) или публичный package; приложение секреты получает только на VM.
VM нужны Python 3.9+, Make, Docker Compose с `--wait`, Git и flock для SSH CD.

`.images/current.json` хранит последний успешный manifest, `previous.json` — предыдущий.
Для отката сначала согласуйте совместимость БД и конфигурации, верните checkout
к `revision` выбранного manifest и выполните `make image-deploy IMAGE_MANIFEST=...`.
Автоматического отката БД/образов нет; не удаляйте нужные digests в GHCR.
`make up` остаётся локальной сборкой из исходников: на VM для релиза используйте
`image-deploy`; `logs`/`ps` читают те же контейнеры. Сторонние образы не пересобираются,
их версии остаются в конфигурации проекта. Внешний registry-кеш между CI runs пока
не настроен. Успешный build не подтверждает runtime: проверяйте readiness и сценарий
сервиса после доставки.

Для диагностики и явных CLI-операций из активного образа используйте
`make image-compose IMAGE_ARGS='ps'`, `IMAGE_ARGS='logs -f'` или
`IMAGE_ARGS='run --rm SERVICE COMMAND'`. Последняя команда автоматически
запрещает сборку и pull. Она подходит для явных миграций/CLI, которые раньше
запускались через локальный Compose-образ. После неуспешной активации
`.images/requested.json` сохраняет manifest попытки; `current.json` остаётся
последним успешным и не гарантирует, что частичный rollout откатился.

Ручной `image-deploy` использует тот же host lock, что SSH CD; не запускайте
обходные Compose-операции одновременно с доставкой. При первом переходе на GHCR
`previous.json` ещё отсутствует: сохраните прежние локальные образы и конфигурацию
для восстановления по runbook до первого релиза.

## Container names

Compose uses explicit service names without replica numbers. Set
`CONTAINER_NAME_PREFIX` (for example `dev-`) for an additional local or CI stack
on the same Docker host. Keep Compose project/service labels and volume names
unchanged; clients address services or explicit network aliases.

## Структура Compose

Файлы Docker Compose находятся в `compose/`; основной файл — `compose/docker-compose.yml`.
Команды запускайте из корня проекта через Makefile: он задаёт `--project-directory`
на корень checkout. Build contexts, bind mounts, `env_file` и секреты остаются
относительными к этому корню; имена Compose project, сервисов, сетей и томов сохранены.
При явном вызове Docker используйте `docker compose --project-directory . -f compose/docker-compose.yml ...`.
Если в приватной `.env` или окружении задан `COMPOSE_FILE`, добавьте `compose/`
к прежним именам файлов, сохранив прежний набор overlays и порядок. Для плавного
перехода Make также распознаёт прежние короткие имена, если файлы уже перенесены.
Не меняйте project name и не удаляйте прежние тома для переноса файлов.
