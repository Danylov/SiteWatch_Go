# SiteWatch

Сервис мониторинга доступности сайтов. Пользователь добавляет URL через веб-интерфейс,
сервис периодически (и по кнопке) проверяет их **параллельно через пул воркеров**,
сохраняет результаты в PostgreSQL и показывает статус, код ответа, задержку и историю.

![stack](https://img.shields.io/badge/Go-1.26-00ADD8) ![db](https://img.shields.io/badge/PostgreSQL-17-336791)

## Быстрый старт

```bash
cp .env.example .env          # настройки
docker compose up -d --wait   # PostgreSQL
go run ./cmd/sitewatch        # миграции применяются автоматически при старте
```

Открыть http://localhost:8080.

Вместо Docker можно использовать локально установленный PostgreSQL: создать роль и базу
`sitewatch` (пароль `sitewatch`) или поменять `DATABASE_URL` в `.env`.

Интеграционные тесты репозитория на реальной БД (`make test-integration`). Тесты очищают таблицы (`TRUNCATE`),
поэтому им нужна **отдельная** база `sitewatch_test` (владелец — `sitewatch`), а не рабочая:

```bash
TEST_DATABASE_URL=postgres://sitewatch:sitewatch@localhost:5432/sitewatch_test?sslmode=disable \
  go test -tags integration ./internal/postgres/
```

## Что где реализовано

| Тема | Где смотреть |
|---|---|
| Роутинг (`net/http.ServeMux` Go 1.22+: методы и `{id}` в путях), middleware | `internal/httpapi/api.go`, `middleware.go` |
| PostgreSQL (pgx/v5, пул соединений, `LEFT JOIN LATERAL`, batch-insert через `unnest`) | `internal/postgres/postgres.go` |
| Миграции (goose, SQL-файлы встроены в бинарник через `embed`) | `migrations/`, `postgres.Migrate` |
| Валидация JSON (`validator/v10`, `DisallowUnknownFields`, лимит тела) | `internal/httpapi/json.go` |
| Централизованная обработка ошибок (handler возвращает `error`, маппинг в HTTP-код в одном месте, `errors.Is/As`) | `internal/httpapi/errors.go` |
| Горутины, буферизированный и небуферизированный каналы | `internal/worker/pool.go` |
| Worker Pool (generic `Run[J, R]`) | `internal/worker/pool.go` |
| `context`: таймаут одной проверки, таймаут всего прогона, graceful shutdown по SIGINT/SIGTERM | `checker.go`, `service.go`, `cmd/sitewatch/main.go` |
| `sync.WaitGroup`, `sync.Mutex` | `pool.go`, `site.Service` (защита от параллельного запуска), `main.go` |
| HTTP-клиент (`http.Client` с собственным `Transport`, лимит редиректов) | `internal/checker/checker.go` |
| JSON (encode/decode, теги, разбор ошибок декодера) | `internal/httpapi/json.go` |
| `.env` (godotenv + типизированный конфиг с валидацией) | `internal/config/config.go` |
| Моки (uber-go/mock, `go:generate`, `go tool mockgen`) | `internal/site/service_test.go`, `internal/httpapi/api_test.go` |
| UI (HTML + JS, встроен в бинарник через `embed`) | `web/static/` |

## Архитектура

```
cmd/sitewatch      точка входа: конфиг, подключение к БД, миграции, HTTP-сервер, graceful shutdown
internal/config    чтение переменных окружения
internal/site      доменные типы, ошибки и бизнес-логика (Service); интерфейсы Repository и Checker
internal/postgres  реализация Repository на pgx
internal/checker   HTTP-проверка URL
internal/worker    универсальный пул воркеров
internal/httpapi   REST API, валидация, обработка ошибок, middleware
migrations         SQL-миграции goose
web                статический UI
```

Зависимости направлены внутрь: `site` ничего не знает о PostgreSQL и HTTP.
Интерфейсы объявлены **на стороне потребителя** (`site.Repository`, `httpapi.SiteService`),
поэтому бизнес-логика и хендлеры тестируются с моками, без базы данных.

### Указатели или значения

- `*Service`, `*Repository`, `*Checker`, `*pgxpool.Pool`, `*http.Client` — указатели:
  у них есть состояние, которое разделяется (мьютекс, пул соединений), копировать их нельзя.
- `Site`, `Check`, `RunSummary`, `Config` — передаются по значению: небольшие неизменяемые
  структуры, копия дешевле, чем выделение в куче и лишняя работа для GC.
  В пул воркеров `Site` уходит копией — у горутин нет общих данных.
- `Site.LastCheck *Check` — указатель, потому что значения может не быть (`nil` — сайт ещё не проверялся).
  Так же в репозитории nullable-колонки `LEFT JOIN` сканируются в указатели.

## API

| Метод | Путь | Описание |
|---|---|---|
| GET | `/api/sites` | список сайтов с последней проверкой |
| POST | `/api/sites` | `{"name": "Go", "url": "https://go.dev"}` → 201 |
| DELETE | `/api/sites/{id}` | 204 / 404 |
| GET | `/api/sites/{id}/checks?limit=20` | история проверок |
| POST | `/api/checks/run` | запустить проверку всех сайтов; 409, если уже идёт |
| GET | `/api/checks/last` | итог последнего прогона |
| GET | `/healthz` | проверка живости |

Формат ошибок единый:

```json
{"error": "validation failed", "fields": {"url": "must be a valid http(s) URL"}}
```

## Тесты

```bash
go test ./...            # unit-тесты (БД не нужна)
go test -race ./...      # проверка на гонки данных
go generate ./...        # перегенерировать моки
```

`-race` требует cgo. На Windows нужен `gcc` (MinGW-w64) в `PATH` и `CGO_ENABLED=1`;
на Linux/macOS всё работает из коробки.

Что покрыто:
- **worker pool** — все задачи обработаны, ограничение параллелизма соблюдается, остановка по отмене контекста;
- **checker** — коды ответа, таймаут, бесконечный редирект (на `httptest.Server`);
- **site.Service** с моками `Repository`/`Checker` — нормализация ввода, подсчёт up/down,
  отказ параллельного запуска (`ErrCheckInProgress`), сохранение результатов после таймаута прогона,
  остановка планировщика;
- **HTTP API** с моком `SiteService` — табличные тесты валидации, маппинг ошибок в коды,
  отсутствие утечки внутренних ошибок, восстановление после паники;
- **config** — значения по умолчанию и ошибки.
