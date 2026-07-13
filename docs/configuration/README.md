# Конфигурация

Только окружение (`internal/config/config.go`, `caarlos0/env`), единый префикс
`GUARDRAILS_` через `env.ParseWithOptions(..., Options{Prefix})`. Вложенные структуры
добавляют свои под-префиксы (`STORE_`, `API_`, `RULES_`, `HEADERS_`, `AUDIT_`; у
policy-структуры `Guardrails` под-префикс намеренно **пустой**, поэтому её поля
читаются как `GUARDRAILS_ENABLED` и т. п.).

Семантика резолюции настроек, модель доверия к заголовкам и логирование — в
[settings.md](settings.md).

## Справочник

| Переменная | По умолчанию | Примечания |
|---|---|---|
| `GUARDRAILS_LOG_LEVEL` | `info` | debug/info/warn/error |
| `GUARDRAILS_LOG_FORMAT` | `json` | `text` для локальной разработки |
| `GUARDRAILS_GRPC_ADDR` | `:9000` | listener ext_proc |
| `GUARDRAILS_GRPC_SECURE` | `false` | self-signed TLS на gRPC-listener (`pkg/tlsutils`). ⚠️ Этот канал несёт исходное немаскированное тело запроса и демаскированный ответ с восстановленными секретами; plaintext безопасен только внутри mTLS-mesh или по loopback — ставьте `true`, если хоп Envoy↔процессор может быть перехвачен |
| `GUARDRAILS_HEALTH_PORT` | `9005` | gRPC health-протокол |
| `GUARDRAILS_METRICS_PORT` | `9091` | Prometheus |
| `GUARDRAILS_ENABLED` | `true` | только seed-значение — см. семантику в settings.md |
| `GUARDRAILS_MODE` | `enforce` | seed-значение; `detect` = shadow-режим: скан + метрики/аудит, тело не изменяется |
| `GUARDRAILS_DATA_TYPES` | `1,2,3,4,5,6` | seed-значение; числа или имена, регистронезависимо. `6`/CUSTOM включён, чтобы кастомные правила из API сканировались по умолчанию; без него каждое кастомное правило молча отключено |
| `GUARDRAILS_KEYWORD_PREFILTER_ENABLED` | `false` | движковый флаг на старте (не хранится, не меняется через API): при `true` пропускает regex правила, если ни одного из его `keywords` нет в тексте. Применяется только к правилам, чей regex гарантирует наличие ключевого слова в каждом совпадении, поэтому **сохраняет полноту** (детекции не меняются) — заметно ускоряет скан. Неподходящие keyword-правила всегда сканируются и перечисляются в стартовом логе |
| `GUARDRAILS_MASK_PARALLEL_MIN_BYTES` | `8192` | движковый флаг на старте (не хранится, не меняется через API): суммарный размер текстов запроса (байты), с которого скан маскирования распараллеливается по полям — вывод маскирования не меняется (fan-out только для скана). Нужно ≥2 текстовых поля; меньшие тела сканируются последовательно, чтобы не платить за горутины на горячем пути. `0` — встроенное значение |
| `GUARDRAILS_PATHS` | 3 стандартных пути → форматы | пары `path:format` через запятую — см. матчинг путей ниже |
| `GUARDRAILS_OVERRIDE_HEADER` | `x-guardrails-data-types` | `""` полностью отключает override |
| `GUARDRAILS_SETTINGS_REFRESH_INTERVAL` | `30s` | `0` отключает |
| `GUARDRAILS_RULES_REFRESH_INTERVAL` | `30s` | `0` отключает |
| `GUARDRAILS_RULES_REGEX_RULES_FILE` | `./configs/guardrails_regex_rules.yaml` | |
| `GUARDRAILS_RULES_GITLEAKS_REGEX_RULES_FILE` | `./configs/guardrails_regex_rules.gitleaks.generated.yaml` | |
| `GUARDRAILS_RULES_MAX_CUSTOM` | `500` | максимум кастомных правил через API (каждое работает на каждом запросе); `0` = без лимита; превышение → 409 |
| `GUARDRAILS_RULES_MAX_PATTERN_LEN` | `4096` | максимум длины regex кастомного правила в байтах; `0` = без лимита; превышение → 400 |
| `GUARDRAILS_HEADERS_DATA_TYPES_HEADER` | `x-guardrails-data-types-triggered` | заголовок ответа; суффикс `-triggered` не конфликтует с override-заголовком запроса |
| `GUARDRAILS_HEADERS_TRIGGERED_RULES_HEADER` | `x-guardrails-triggered-rules` | |
| `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES` | `false` | ID правил раскрывают детекторы → opt-in |
| `GUARDRAILS_STORE_BACKEND` | `in_memory` | `redis` / `postgres` |
| `GUARDRAILS_STORE_MASKING_TTL` | `15m` | должен превышать самый длинный стриминговый ответ |
| `GUARDRAILS_STATE_KEY_SALT` | — | соль ключа masking state `HMAC-SHA256(salt, x-request-id)`; общая для реплик; пусто = SHA-256 без соли + предупреждение; не логируется |
| `GUARDRAILS_STATE_DELETE_ON_CLOSE` | `true` | `false` сохраняет состояние для реплик с раздельными запросом/ответом (TTL освободит) |
| `GUARDRAILS_STORE_REDIS_ADDR` | `redis:6379` | |
| `GUARDRAILS_STORE_REDIS_PASSWORD` | — | |
| `GUARDRAILS_STORE_REDIS_DB` | `0` | |
| `GUARDRAILS_STORE_POSTGRES_DSN` | — | DSN пула pgx |
| `GUARDRAILS_STORE_ENCRYPTION_ENABLED` | `false` | AES-256-GCM-шифрование masking state в redis/postgres на месте (no-op для in_memory) — см. [storage/](../storage/) |
| `GUARDRAILS_STORE_ENCRYPTION_KEY` | — | base64 32-байтный ключ (`openssl rand -base64 32`); обязателен при включённом шифровании, неверный/отсутствующий ключ ломает старт; не логируется |
| `GUARDRAILS_API_ADDR` | `:9080` | management REST API (grpc-gateway); `""` отключает весь management API, и REST, и gRPC |
| `GUARDRAILS_API_GRPC_ADDR` | `:9090` | management gRPC API (`GuardrailsApi`), отдельный от ext_proc-listener на `GUARDRAILS_GRPC_ADDR`/`:9000` |
| `GUARDRAILS_AUDIT_ENABLED` | `false` | пофазовый аудит маскирования + эндпоинты `/v1/audit` |
| `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS` | `false` | дополнительно хранить маскированные (с подставленными плейсхолдерами) тексты запроса — это пользовательский контент, см. чеклист в [operations/](../operations/) |
| `GUARDRAILS_AUDIT_RETENTION` | `24h` | TTL аудит-записи в хранилище |
| `GUARDRAILS_AUDIT_MAX_ENTRIES` | `10000` | только для in_memory: лимит карты аудита, вытесняется старейшее; `0` = без лимита |

## Матчинг путей (`GUARDRAILS_PATHS`)

По умолчанию: `/v1/chat/completions:chat_completions,/v1/messages:messages,/v1/responses:responses`.

- Синтаксис значения: пары `path:format` через запятую. Допустимые форматы:
  `chat_completions`, `messages`, `responses`. Путь не может содержать литеральный `:`
  (разделитель env-карты) — экзотический, но легальный символ URL, задокументированное
  ограничение.
- Матчинг (`models.PathResolver`): query-строка отбрасывается, затем точное
  совпадение, затем самый **длинный** сконфигурированный ключ, являющийся суффиксом
  пути. Ключи должны начинаться с `/`, что якорит суффиксный матч на границе сегмента:
  `/openai/v1/messages` совпадает с ключом `/v1/messages`, а `/xv1/messages` и
  `/v1/messages/foo` — нет. Проксирующим монтированиям настройка не нужна.
- Сконфигурированные записи **подмешиваются поверх карты по умолчанию** (запись
  пользователя для того же пути побеждает). Поэтому ключевые эндпоинты остаются под
  защитой даже при частичном `GUARDRAILS_PATHS` — добавление прокси-монтирования не
  может молча снять маскирование с `/v1/chat/completions` и др.
- Валидация на старте (`config.Load` и `extproc.New`): неизвестный формат, ключ без
  ведущего `/` или пустая карта ломают старт. Ошибка конфигурации путей не должна
  молча приводить к 400 на всём трафике в рантайме.
