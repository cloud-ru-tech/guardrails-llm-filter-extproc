<div align="center">

<img src="docs/images/logo.svg" width="84" alt="Cloud.ru Guardrails" />

# guardrails-llm-filter-extproc

**Маскирование PII и секретов в трафике к LLM — как Envoy external processor.**

Тот же движок маскирования, что и в [`guardrails-llm-filter`](https://github.com/cloud-ru-tech/guardrails-llm-filter),
но упакованный как gRPC-сайдкар Envoy `ext_proc`: Envoy остаётся data-plane, а фильтр
вызывается по gRPC.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Release](https://img.shields.io/badge/release-v0.1.0-2ea44f)](CHANGELOG.md)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)
[![Made by Cloud.ru](https://img.shields.io/badge/made%20by-Cloud.ru-26D07C)](https://cloud.ru)

</div>

---

`guardrails-llm-filter-extproc` стоит в data-path вашего LLM-шлюза. На пути к модели он
сканирует тела запросов набором regex-правил детекции (~260 встроенных: учётные данные,
API-ключи, access-токены, IP-адреса, персональные данные) и заменяет найденные значения
синтетическими плейсхолдерами вида `<EMAIL_1>`. На обратном пути восстанавливает
оригиналы — прозрачно для клиента, включая потоковую передачу токен-за-токеном (SSE).

```
client ──► Envoy ──[маскирование]──► LLM-провайдер
   ▲          │ ext_proc (gRPC)
   │          ▼
   └──[демаскирование]── guardrails-llm-filter-extproc
```

LLM-провайдер никогда не видит чувствительные значения; клиент никогда не видит
плейсхолдеры.

## Возможности

- 🛡️ **~260 встроенных правил** детекции: учётные данные, API-ключи, access-токены,
  IP-адреса, персональные данные (российские PII, карты, IBAN, СНИЛС/ИНН/ОГРН — с
  валидаторами контрольных сумм).
- 🔄 **Прозрачное демаскирование** ответа, включая потоковые SSE токен-за-токеном и
  аргументы tool-call.
- 🔌 **OpenAI и Anthropic из коробки**: `/v1/chat/completions`, `/v1/responses`,
  `/v1/messages` — JSON и стриминг.
- 🧩 **Envoy-native**: bidi-стрим `ext_proc` v3, buffered-запрос + full-duplex-стриминг
  ответа, `failure_mode_allow`.
- 🟢 **Fail-open по замыслу**: любая внутренняя ошибка пропускает трафик, а не ломает его.
- 👁️ **Detect (shadow) режим**, переключаемый через API без передеплоя.
- 🗄️ **Хранилища**: `in_memory` / `redis` / `postgres` с межрепличным masking state и
  опциональным AES-256-GCM-шифрованием на месте.
- 📊 **Наблюдаемость**: метрики Prometheus, дашборд Grafana, gRPC health, аудит-трейл.

## Веб-консоль управления

У движка есть готовая веб-консоль (обзор срабатываний, правила, песочница, настройки,
журнал аудита) — она **поставляется из коробки в standalone-репозитории**
[`guardrails-llm-filter`](https://github.com/cloud-ru-tech/guardrails-llm-filter) и общается с тем же контрактным
`GuardrailsApi`, что реализует и этот сервис. Для extproc её можно **поднять отдельно и
направить на management REST API этого сервиса** (`GUARDRAILS_API_ADDR`, по умолчанию
`:9080`):

```sh
# в репозитории guardrails-llm-filter:
cd frontend
GUARDRAILS_API_URL=http://localhost:9080 npm run dev   # консоль на http://localhost:5173
```

Так как правила, настройки и аудит хранятся и отдаются через общий `GuardrailsApi`, консоль
управляет extproc-инстансом ровно как standalone (страницы «Обзор» и «Аудит» наполняются
при `GUARDRAILS_AUDIT_ENABLED=true`). Ниже — как это выглядит:

<div align="center">

<img src="docs/images/overview.png" width="820" alt="Дашборд «Обзор»" />

<br/>

<img src="docs/images/audit-detail.png" width="820" alt="Журнал аудита — деталь записи" />

</div>

> Скриншоты сняты в standalone-сервисе; UI одинаков — меняется лишь адрес management API,
> на который он смотрит. Полный набор экранов и вшитая в бинарь консоль — в
> [`guardrails-llm-filter`](https://github.com/cloud-ru-tech/guardrails-llm-filter). Консоль и config API работают
> **только внутри кластера** — никогда публичный ingress.

## Быстрый старт

```sh
cd examples/quickstart
docker compose up --build
# в другом терминале:
bash demo.sh
```

Демо шлёт промпты с фейковыми email и номером карты через Envoy (`localhost:10000`),
показывает маскированный текст в логах mock-LLM и демаскированный ответ у клиента, затем
добавляет кастомное правило через config API и показывает его в действии.

## Конфигурация Envoy

Фильтр ext_proc **обязан** использовать эти режимы обработки (полный конфиг — в
`examples/quickstart/proxy-config.yaml`):

```yaml
processing_mode:
  request_header_mode: SEND
  response_header_mode: SEND
  request_body_mode: BUFFERED              # запрос маскируется целиком
  response_body_mode: FULL_DUPLEX_STREAMED # ответ демаскируется потоком
  response_trailer_mode: SEND              # ОБЯЗАТЕЛЬНО для full-duplex тел
```

Дополнительные требования:

- Кластер ext_proc по умолчанию говорит **plaintext gRPC** на порту 9000
  (`GUARDRAILS_GRPC_SECURE=true` включает self-signed TLS-listener). Этот хоп несёт
  исходное немаскированное тело и демаскированный ответ с восстановленными секретами,
  поэтому plaintext безопасен только внутри mTLS-mesh или по loopback — включайте TLS,
  если хоп может быть перехвачен.
- **Вырезайте override-заголовок из клиентских запросов** — см.
  [Per-request override](#per-request-override).
- **`x-request-id` должен генерироваться шлюзом, а не контролироваться клиентом.** Он —
  ключ masking state во внешних хранилищах; с общим хранилищем и раздельной топологией
  запрос/ответ клиент, способный подделать живой `x-request-id` жертвы, мог бы
  демаскировать оригиналы жертвы в свой ответ. Envoy генерирует его по умолчанию —
  убедитесь, что граница доверия его регенерирует (не сохраняйте клиентское значение
  вслепую).
- Опционально: заголовок запроса `x-gateway-model-name` подхватывается как поле лога.

## Конфигурация

Все переменные с префиксом `GUARDRAILS_`.

| Переменная | По умолчанию | Описание |
|---|---|---|
| `GUARDRAILS_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `GUARDRAILS_LOG_FORMAT` | `json` | `json` \| `text` |
| `GUARDRAILS_GRPC_ADDR` | `:9000` | адрес gRPC-listener ext_proc |
| `GUARDRAILS_GRPC_SECURE` | `false` | TLS (self-signed) на gRPC-listener. ⚠️ несёт немаскированные тела + восстановленные секреты — plaintext безопасен только в mTLS-mesh/loopback |
| `GUARDRAILS_HEALTH_PORT` | `9005` | порт gRPC health-сервиса |
| `GUARDRAILS_METRICS_PORT` | `9091` | порт метрик Prometheus |
| `GUARDRAILS_ENABLED` | `true` | глобальный вкл/выкл (seed-значение, см. ниже) |
| `GUARDRAILS_MODE` | `enforce` | `detect` = shadow-режим: скан + метрики/аудит, трафик не тронут (seed) |
| `GUARDRAILS_DATA_TYPES` | `1,2,3,4,5,6` | включённые типы данных, числа или имена (`6`/CUSTOM включает кастомные правила из API — без него они молча не сканируются) |
| `GUARDRAILS_KEYWORD_PREFILTER_ENABLED` | `false` | пропускать regex правила, если ни одного из его `keywords` нет в тексте — только для правил, чей regex гарантирует ключевое слово в каждом совпадении, поэтому полнота сохраняется; заметное ускорение скана |
| `GUARDRAILS_MASK_PARALLEL_MIN_BYTES` | `8192` | суммарный размер текстов (байты), с которого скан распараллеливается по полям (нужно ≥2 поля); `0` — встроенное значение |
| `GUARDRAILS_PATHS` | 3 стандартных пути | пары `path:format` (`chat_completions`, `messages`, `responses`); суффиксный матчинг, подмешиваются поверх дефолтов |
| `GUARDRAILS_OVERRIDE_HEADER` | `x-guardrails-data-types` | per-request заголовок сужения; пусто отключает |
| `GUARDRAILS_SETTINGS_REFRESH_INTERVAL` | `30s` | интервал перечитывания настроек (сходимость реплик); `0` отключает |
| `GUARDRAILS_RULES_REFRESH_INTERVAL` | `30s` | интервал перечитывания кастомных правил; `0` отключает |
| `GUARDRAILS_RULES_REGEX_RULES_FILE` | `./configs/guardrails_regex_rules.yaml` | ручной файл правил |
| `GUARDRAILS_RULES_GITLEAKS_REGEX_RULES_FILE` | `./configs/guardrails_regex_rules.gitleaks.generated.yaml` | генерируемый файл правил |
| `GUARDRAILS_HEADERS_DATA_TYPES_HEADER` | `x-guardrails-data-types-triggered` | заголовок ответа со сработавшими типами данных |
| `GUARDRAILS_HEADERS_TRIGGERED_RULES_HEADER` | `x-guardrails-triggered-rules` | заголовок ответа со сработавшими ID правил |
| `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES` | `false` | эмитить заголовок сработавших правил |
| `GUARDRAILS_STORE_BACKEND` | `in_memory` | `in_memory` \| `redis` \| `postgres` |
| `GUARDRAILS_STORE_MASKING_TTL` | `15m` | страховочный TTL masking state; должен превышать самый длинный стриминговый ответ |
| `GUARDRAILS_STATE_KEY_SALT` | — | соль ключа masking state (`HMAC-SHA256(salt, x-request-id)`); задайте общий секрет для реплик; пусто = SHA-256 без соли + предупреждение; не логируется |
| `GUARDRAILS_STATE_DELETE_ON_CLOSE` | `true` | удалять masking state при закрытии потока; `false` для реплик с раздельными запросом/ответом (TTL освободит) |
| `GUARDRAILS_STORE_REDIS_ADDR` | `redis:6379` | адрес redis-бэкенда |
| `GUARDRAILS_STORE_REDIS_PASSWORD` | — | пароль redis |
| `GUARDRAILS_STORE_REDIS_DB` | `0` | база redis |
| `GUARDRAILS_STORE_POSTGRES_DSN` | — | DSN postgres |
| `GUARDRAILS_STORE_ENCRYPTION_ENABLED` | `false` | AES-256-GCM-шифрование masking state в `redis`/`postgres` на месте (no-op для `in_memory`) |
| `GUARDRAILS_STORE_ENCRYPTION_KEY` | — | base64 32-байтный ключ (`openssl rand -base64 32`); обязателен при включённом шифровании — неверный/отсутствующий ключ ломает старт |
| `GUARDRAILS_API_ADDR` | `:9080` | адрес config REST API (grpc-gateway); пусто отключает весь management API, REST и gRPC |
| `GUARDRAILS_API_GRPC_ADDR` | `:9090` | management gRPC-адрес (`GuardrailsApi`), отдельный от ext_proc-listener на `GUARDRAILS_GRPC_ADDR`/`:9000` |
| `GUARDRAILS_AUDIT_ENABLED` | `false` | пофазовый [аудит маскирования](#audit-trail) + эндпоинты `/v1/audit` |
| `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS` | `false` | дополнительно хранить маскированные тексты запроса (пользовательский контент — см. заметку о безопасности) |
| `GUARDRAILS_AUDIT_RETENTION` | `24h` | сколько хранятся аудит-записи |
| `GUARDRAILS_AUDIT_MAX_ENTRIES` | `10000` | только для `in_memory`: лимит аудит-записей (вытесняется старейшее) |

Типы данных: `1 CREDENTIALS`, `2 API_KEYS`, `3 ACCESS_TOKENS`, `4 IP_ADDRESSES`,
`5 PERSONAL_DATA`, `6 CUSTOM`. Имена принимаются регистронезависимо всюду, где принимаются
числа.

### Бэкенды хранилища

Хранилище держит четыре вещи: per-request **masking state** (маппинг
плейсхолдер→оригинал для демаскирования ответов), **кастомные правила**, созданные через
API, **глобальные настройки** и опциональные **аудит-записи**.

| Бэкенд | Masking state | Кастомные правила / настройки переживают рестарт | Много реплик |
|---|---|---|---|
| `in_memory` (по умолчанию) | в процессе | нет — база это env + YAML-файлы | нет |
| `redis` | общий, с TTL | да | да |
| `postgres` | общий, с TTL | да | да |

> **Заметка о безопасности**: masking state содержит *исходные* чувствительные значения.
> С `redis`/`postgres` они покидают память процесса — ограничьте доступ к хранилищу,
> держите короткий TTL. Записи удаляются при закрытии потока; TTL — страховка. См.
> [SECURITY.md](SECURITY.md).

`in_memory` полностью корректен для одиночной реплики, потому что Envoy обрабатывает
запрос и его ответ на одном ext_proc-потоке. Общий бэкенд добавляет межрепличную
устойчивость и durable кастомные правила.

### Глобальные настройки и per-request override

<a name="per-request-override"></a>Env-значения засевают хранилище настроек на первом
старте; далее источник истины — `GET/PUT /v1/settings` (перечитывается каждые
`SETTINGS_REFRESH_INTERVAL` на всех репликах). Per-request доверенный шлюз может задать
override-заголовок (по умолчанию `x-guardrails-data-types`), чтобы **сузить** включённые
проверки:

```
x-guardrails-data-types: credentials,api_keys   # только эти (∩ глобальные)
x-guardrails-data-types: none                   # пропустить guardrails
```

Override — только пересечение: он никогда не включит глобально выключенный тип данных.
Неразбираемые значения полностью игнорируются (действует полная защита). **Envoy обязан
удалять этот заголовок из входящих клиентских запросов**:

```yaml
request_headers_to_remove: [x-guardrails-data-types]
```

## Config API

Контракт-первичный: management API определён proto-первично в
[`api/proto/cloudru/guardrails/v1/service`](api/proto/cloudru/guardrails/v1/service)
(сервис `GuardrailsApi`) и генерируется `make gen-proto` в gRPC-сервер на
`GUARDRAILS_API_GRPC_ADDR` (по умолчанию `:9090`) плюс REST-прокси grpc-gateway на
`GUARDRAILS_API_ADDR` (по умолчанию `:9080`) — объединённая спека OpenAPI v2 это
[`service.swagger.json`](service.swagger.json). API **неаутентифицирован** — защищайте на
сетевом уровне (только внутри кластера, никогда публичный ingress). Именно этот API
использует [веб-консоль](#веб-консоль-управления).

```
GET    /v1/rules?source=all|builtin|custom
POST   /v1/rules
GET    /v1/rules/{id}
PUT    /v1/rules/{id}        # только кастомные правила
PATCH  /v1/rules/{id}        # вкл/выкл любое правило, включая builtin: {"enabled": false}
DELETE /v1/rules/{id}        # только кастомные правила
GET    /v1/settings
PUT    /v1/settings
GET    /v1/data-types
GET    /v1/audit/records                 # аудит-трейл (GUARDRAILS_AUDIT_ENABLED=true)
GET    /v1/audit/records/{request_id}
```

Пример — добавить кастомное правило детекции (валидируется тем же путём компиляции, что и в
проде, применяется атомарно без рестарта, сохраняется в хранилище):

```sh
curl -X POST localhost:9080/v1/rules -H 'Content-Type: application/json' -d '{
  "rule_id": "acme_token",
  "name": "ACME internal token",
  "data_type": 6,
  "regex": "\\bacme-[0-9a-f]{8}\\b",
  "keywords": ["acme-"],
  "masking": {"placeholder": "ACME_TOKEN"}
}'
```

Не забудьте включить тип данных 6 (`custom`) в настройках, чтобы такие правила работали.

## Файлы правил

Встроенные правила лежат в двух YAML-файлах, загружаемых на старте:

- `configs/guardrails_regex_rules.yaml` — ручные (российские PII, платёжные карты, IBAN,
  email'ы, телефоны, IP, …) с валидаторами контрольных сумм (Luhn, СНИЛС, ИНН, ОГРН,
  IBAN mod-97).
- `configs/guardrails_regex_rules.gitleaks.generated.yaml` — сгенерированы из набора
  [gitleaks](https://github.com/gitleaks/gitleaks) (`make rules-gen` перегенерирует из
  `configs/gitleaks.toml`).

Схема правила (та же, что в API):

```yaml
guardrails_regex_rules:
  - data_type: 5
    name: personal_data
    rules:
      - rule_id: email
        name: Email address
        regex: '[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}'
        keywords: ["@"]          # дешёвый пре-фильтр перед запуском regex
        validators: [email_ascii]
        masking:
          placeholder: EMAIL     # найденные значения становятся <EMAIL_1>, <EMAIL_2>…
```

## Наблюдаемость

- **Метрики**: Prometheus на `:9091/metrics`, префикс `extproc_guardrails_` (длительности
  pipeline/mask/demask, счётчики срабатываний правил и типов данных, счётчик маскированных
  запросов, сбои, сбои хранилища).
- **Дашборд**: импортируемый дашборд Grafana — в
  [`deploy/grafana/dashboard.json`](deploy/grafana/dashboard.json) (использует input
  datasource `DS_PROMETHEUS`).
- **Алертинг**: пример правил Prometheus в
  [`deploy/prometheus/guardrails-llm-filter-extproc-alerts.yml`](deploy/prometheus/guardrails-llm-filter-extproc-alerts.yml);
  для prometheus-operator есть opt-in kustomize-компонент с `ServiceMonitor` +
  `PrometheusRule` в [`deploy/kubernetes/components/monitoring/`](deploy/kubernetes/components/monitoring/).
- **Health**: gRPC health-протокол на `:9005` (используется k8s-пробами в
  `deploy/kubernetes`).
- **Заголовки ответа**: `x-guardrails-data-types-triggered` перечисляет сработавшие типы
  данных; `x-guardrails-triggered-rules` (opt-in через
  `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES`) перечисляет ID правил.

### Аудит-трейл

<a name="audit-trail"></a>`GUARDRAILS_AUDIT_ENABLED=true` записывает на каждый
маскированный запрос, какие правила и типы данных сработали и какие плейсхолдеры заменили
найденные значения — с запросом по request id или с фильтрами через
`GET /v1/audit/records`. Записи **никогда не содержат исходных значений**; с
`GUARDRAILS_AUDIT_STORE_MASKED_TEXTS=true` дополнительно включают маскированные тексты
запроса (с подставленными плейсхолдерами). Retention — `GUARDRAILS_AUDIT_RETENTION`;
записи асинхронны и fail-open. Эти же записи наполняют страницы «Обзор» и «Аудит»
[веб-консоли](#веб-консоль-управления).

```sh
curl 'localhost:9080/v1/audit/records?data_type=personal_data&limit=10'
```

> Маскированные тексты — всё ещё пользовательский контент промпта. Включайте
> `STORE_MASKED_TEXTS` только с хранилищем под контролем доступа и config API,
> достижимым лишь внутри кластера (никогда публичный ingress).

## Деплой

- Docker-образ: `make docker-build` (multi-stage, distroless, non-root).
- Kubernetes: `deploy/kubernetes/` (kustomize-база: deployment, service, configmap,
  secret).

## Разработка

```sh
make build       # бинарь в ./bin
make test        # нужен Docker для postgres-conformance
make test-short  # без Docker
make lint        # golangci-lint
make demo-up     # полное сквозное демо
```

Структура проекта: `cmd/` — точки входа, `internal/controller/extproc` — процессор Envoy,
`internal/controller/api` — контракт-первичный config API (gRPC + grpc-gateway),
`internal/repository` — бэкенды персистентности (+ общий conformance-набор),
`pkg/guardrails/regex` — переиспользуемый движок детекции (правила, реестр, сканеры,
валидаторы). Подробная инженерная документация — в [`docs/`](docs/README.md). См. также
[CONTRIBUTING.md](CONTRIBUTING.md).

## Лицензия

Apache-2.0 — см. [LICENSE](LICENSE). Включает правила, производные от
[gitleaks](https://github.com/gitleaks/gitleaks) (MIT), см. [NOTICE](NOTICE).
