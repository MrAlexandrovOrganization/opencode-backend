# opencode-backend

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

Локально:

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
- `GET /api/v1/sessions/{id}/activity` — живой статус сессии (занята ли,
  текущий инструмент/статус, накопленный текст, ожидающие разрешения и вопросы,
  статус opencode-сервера `idle`/`busy`/`retry`).
- `POST /api/v1/sessions/{id}/messages` (202 + messageID), `GET …/messages`,
  `GET …/messages/{mid}` — сообщения.
- `POST /api/v1/sessions/{id}/abort` — прервать запрос.
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