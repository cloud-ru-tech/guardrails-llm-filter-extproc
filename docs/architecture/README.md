# Архитектура

```
                    ┌─────────────────────── guardrails-llm-filter-extproc ─────────────────────┐
client ──► Envoy ───┤ :9000 gRPC ext_proc      (data-path: маскирование / демаскирование)       │
   ▲          │     │ :9090 gRPC mgmt API      :9080 HTTP mgmt API (grpc-gateway REST)          │
   │          ▼     │ :9005 gRPC health        :9091 Prometheus                                 │
   └── ответ ───────┤                                                                           │
                    │  settings.Service ◄──┐                                                    │
                    │  rules.UseCase ──────┼──► repository.Store (in_memory | redis | postgres) │
                    │  registry.Reloadable ┘        ▲                                           │
                    │  (скомпилированные правила)   └── masking state (запрос→ответ)            │
                    └───────────────────────────────────────────────────────────────────────────┘
                                             │
                                             ▼
                                        LLM-провайдер
```

Management API определяется контрактом (proto → gRPC + grpc-gateway REST): `:9090` —
gRPC-сервис `GuardrailsApi`, `:9080` — REST-прокси grpc-gateway перед ним,
сгенерированный из того же контракта (те же маршруты и JSON-формы, другой транспорт).

Подробности по разделам:
- [components.md](components.md) — карта пакетов и зон ответственности.
- [request-lifecycle.md](request-lifecycle.md) — жизненный цикл запроса через ext_proc-поток.

## Модуль

`github.com/cloud-ru-tech/guardrails-llm-filter-extproc`, Go 1.26. Приватных
зависимостей нет; заметные публичные: `envoyproxy/go-control-plane` (типы
ext_proc), `tidwall/gjson+sjson` (извлечение/патч JSON), `redis/go-redis/v9`,
`jackc/pgx/v5`, `caarlos0/env/v11`, `goccy/go-yaml`, middleware и prometheus из
`grpc-ecosystem`, `anthropics/anthropic-sdk-go` (только типы ответов). Только для
тестов: `stretchr/testify`, `alicebob/miniredis/v2`, `testcontainers-go`,
`go.uber.org/mock`.

## Сборка зависимостей (`internal/app/app.go`)

`Extproc` — структура из ленивых мемоизированных геттеров: каждый `X()` строит
объект один раз и кэширует. Геттеры **паникуют при некорректной конфигурации на
старте** (битый файл правил, недоступный redis/postgres, неверный
`GUARDRAILS_DATA_TYPES`) — это осознанно: неправильная конфигурация должна
убивать pod, а не деградировать молча. Ключевые геттеры:

- `Store()` — фабрика + `Ping` (таймаут 15 с) для внешних бэкендов.
- `loadFileRules()` / `DataTypes()` — YAML-файлы парсятся один раз.
- `GuardrailsRegistry()` — правила из файлов компилируются в `registry.Reloadable`.
- `SettingsService()` — env-дефолты через `settings.ParseDataTypes`.
- `RulesUseCase()` / `BuiltinsIndex()` / `RulesReloader()` — правила из файлов + хранилище.
- `ExtprocController()` — получает Masker, SettingsService, DemaskerProvider, Store.
- `GrpcController(ctx)` — реализация gRPC `GuardrailsApi` (`internal/controller/api`).
- `ManagementGrpcServer(ctx)` — gRPC-сервер для `GrpcController` на `GUARDRAILS_API_GRPC_ADDR`.
- `APIServer(ctx)` — REST-прокси grpc-gateway перед `ManagementGrpcServer`; `nil`,
  когда `GUARDRAILS_API_ADDR` пуст. API неаутентифицирован по замыслу и пишет об
  этом предупреждение при старте.

`Start(ctx)`:
1. `SettingsService().Load` (засевает хранилище env-дефолтами, если оно пустое) и
   `RulesReloader().Reload` (подмешивает кастомные правила из хранилища) — обе
   ошибки **только логируются** (fail-open-старт: работаем на базе env/файлов,
   тикеры починят позже).
2. Привязка listener'ов, запуск горутин: ext_proc gRPC, health gRPC, metrics HTTP,
   management gRPC, config API HTTP (grpc-gateway).
3. Запуск тикеров обновления (`RunRefresh`) на отдельном контексте.
4. Сохранение closure для остановки в `e.stop`; `Stop()` выполняет его (бюджет 10 с):
   отмена тикеров → shutdown API (grpc-gateway) + metrics → GracefulStop health +
   ext_proc-grpc + management-grpc → дренаж аудит-рекордера → `store.Close()`.

Цепочка gRPC-перехватчиков (unary и stream): recovery (паники → `codes.Internal` +
slog со значением паники) → grpc logging (адаптер slog через
`grpclogging.LoggerFunc`) → prometheus → protovalidate.

## Инварианты (нарушать нельзя)

1. **Никогда не блокировать** — вердикт это маскировать/пропустить (enforce) или
   только просканировать и пропустить (detect-режим: метрики + аудит, тело не
   тронуто, состояние не сохраняется, демаскирования нет). Вердикта «заблокировать»
   в пайплайне не существует.
2. **Fail-open на data-path** — ошибки маскирования, хранилища, перезагрузки:
   лог + метрика + пропуск/удержание последнего валидного снимка. В связке с Envoy
   `failure_mode_allow` это задаёт общую позицию отказоустойчивости.
3. **Никаких чувствительных значений в логах** — `MaskingState.Replacements` хранит
   оригиналы; их нельзя логировать. Старые отладочные пути, печатавшие демаскированные
   значения, были удалены как исправление безопасности; возвращать их нельзя.
4. **Только сужение через override** — заголовок запроса может лишь пересекаться с
   глобальными типами данных, но не расширять их; мусор на входе → заголовок
   полностью игнорируется (полная защита).
5. **Единственный путь валидации правил** — всё (загрузка файлов, create/update
   через API) компилируется через `registry.CompileRule`; второй реализации
   валидации быть не должно.
6. **Чтения снимка** — потребители data-path читают реестр через `Reloadable`;
   расхождение снимков внутри запроса допустимо (удалённое правило деградирует до
   «не применено»), рваный/частичный набор правил — нет.
