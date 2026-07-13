# Разработка

## Команды

```sh
make build            # ./bin/guardrails-llm-filter-extproc
make run              # go run ./cmd/guardrails-llm-filter-extproc
make test             # go test -race ./...   (postgres-conformance нужен Docker, ~60s; без него авто-скип)
make test-short       # -short: пропускает тесты, зависящие от Docker
make lint             # golangci-lint run (конфиг: .golangci.yml, формат v2)
make generate         # go generate ./... (go-enum должен быть в PATH для internal/models)
make rules-gen        # перегенерировать gitleaks-YAML правил из configs/gitleaks.toml
make docker-build     # локальный образ guardrails-llm-filter-extproc:local
make demo-up / demo-down   # quickstart-стек compose

go test ./internal/controller/extproc/ -run TestHandleRequestHeadersOverrideNarrows   # один тест
```

CI: `.github/workflows/ci.yml` — build + `go test -race ./...`, golangci-lint, docker
build (без push).

## Стратегия тестирования (что есть, где)

| Область | Тесты |
|---|---|
| бэкенды хранилища | conformance-набор `internal/repository/repositorytest`; memory напрямую, redis через **miniredis** (TTL через `FastForward`), postgres через **testcontainers** (скип под `-short`) |
| настройки | табличные тесты для `Effective` (сужение, `none`, мусор→игнор, имена+числа) и `Service` (seed/предпочесть сохранённое/update/сходимость refresh) |
| сервис правил | матрица валидации, конфликты duplicate/builtin, shadow-skip при reload, конкурентный Create (-race) |
| реестр | ошибочные кейсы `Build`; замена `Reloadable` + race-тест конкурентных читателей/писателя (согласованность утверждается **внутри одного снимка** — межвызовный дрейф by design) |
| пайплайн | `internal/controller/extproc/pipeline_test.go` — фейки SettingsProvider/Masker/StateStore: сужение override, skip при disabled, запись state, fail-open при ошибке Put, fallback хранилища в фазе ответа, удаление state в Close |
| HTTP API | `httptest` против реального mux + реальных сервисов + memory-store: CRUD, маппинг 400/404/409, настройки имена+числа |
| движок | юнит-тесты сканера/валидатора/загрузчика + data-driven корпус правил в `tests/rules` (кейсы для новых правил добавляйте сюда) |
| SSE | тесты процессоров в `internal/sseproc/{chatcompletions,messages}` |

Моки: `go.uber.org/mock`, паттерн `//go:generate mockgen -source=contract.go` в пакетах
mask/demask.

## Генерируемые файлы — не править руками

- `internal/models/data_type_enum.go` ← go-enum из ENUM-комментария в `data_type.go`
  (`cd internal/models && go generate ./...`).
- `configs/guardrails_regex_rules.gitleaks.generated.yaml` ← `make rules-gen`.

## Демо quickstart (`examples/quickstart`)

`docker compose up --build`: Envoy (:10000, admin :9901) с правильными режимами ext_proc
→ guardrails-llm-filter-extproc (собран из корня репо) → `mock-llm` (крошечный
OpenAI-совместимый echo-сервер, который логирует то, что получил, — т. е. маскированный
текст — и отдаёт обратно, JSON или SSE). `bash demo.sh` прогоняет сценарий:
mask/demask без стриминга, SSE, добавление кастомного правила через API и его
срабатывание. Это самый быстрый сквозной способ проверить изменение data-path: лог mock
должен показывать плейсхолдеры, ответ клиенту — оригиналы, `x-guardrails-data-types-triggered`
должен присутствовать.

## Подводные камни

- **Режимы тел Envoy асимметричны** — запрос BUFFERED, ответ FULL_DUPLEX_STREAMED
  (+ `response_trailer_mode: SEND`). Ошибка здесь даёт пустое тело на upstream (HTTP 400
  «EOF» от Go-upstream).
- Префикс `env.ParseWithOptions`: добавление поля конфига означает имя env
  `GUARDRAILS_` + под-префикс + тег. Следите за случайным удвоением `GUARDRAILS_GUARDRAILS_*`.
- Паники в геттерах `internal/app` на старте намеренны (fail-fast на misconfig);
  рантайм-пути должны оставаться fail-open.
- Используются стандартные пакеты `slices`/`maps` и паттерны `http.NewServeMux` из Go 1.22;
  держитесь современных идиом.
