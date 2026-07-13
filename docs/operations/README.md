# Эксплуатация

## Порты

| Порт | Протокол | Назначение |
|---|---|---|
| 9000 | gRPC | data-path ext_proc (по умолчанию plaintext; `GRPC_SECURE=true` → self-signed TLS) |
| 9005 | gRPC | health-протокол (`grpc_health_v1`) — используется k8s-пробами |
| 9090 | gRPC | management API (`GuardrailsApi`), `GUARDRAILS_API_GRPC_ADDR` |
| 9080 | HTTP | management API, REST-прокси grpc-gateway перед :9090 (оба отключаются пустым `GUARDRAILS_API_ADDR`) |
| 9091 | HTTP | Prometheus `/metrics`, `GUARDRAILS_METRICS_PORT` |

## Метрики (namespace `extproc_guardrails_`)

| Метрика | Тип / метки | Смысл |
|---|---|---|
| `pipeline_duration_seconds` | histogram | суммарное время mask+demask на запрос |
| `mask_duration_seconds` | histogram | маскирование запроса + мутация тела |
| `mask_scan_duration_seconds` / `scan_duration_seconds` | histogram | regex-скан, все тексты / на текст |
| `mask_texts_count`, `mask_scan_text_bytes`, `mask_scan_total_bytes` | histogram | объём скана |
| `demask_duration_seconds` | histogram | демаскирование полного (non-SSE) ответа |
| `sse_chunk_demask_duration_seconds` | histogram | на SSE-кусок |
| `triggered_rules_count` | histogram | различных правил на запрос |
| `rule_triggers_total` | counter `{rule_id}` | срабатывания правил на уровне запроса |
| `data_type_triggers_total` | counter `{data_type}` | срабатывания типов данных на уровне запроса |
| `requests_masked_total` | counter `{mode=enforce\|detect}` | запросы, где хотя бы одно значение было замаскировано (enforce) или было бы (detect); чистый сигнал на запрос для алертинга — счётчики срабатываний переоценивают запросы с несколькими триггерами. Вид shadow-раскатки: `sum by (mode) (rate(...[5m]))` |
| `mask_failures_total` | counter | ошибки сценария маскирования (fail-open пропуск) |
| `demask_failures_total` | counter `{mode=full\|sse}` | сбои/откаты демаскирования |
| `masking_state_store_failures_total` | counter `{op=put\|get\|delete\|decrypt}` | fail-open ошибки хранилища; `decrypt` = недешифруемая запись (неверный/ротированный ключ, или шифрование выключено при наличии зашифрованных записей) |
| `audit_store_failures_total` | counter `{op=put\|get\|list}` | ошибки хранилища аудита (put = fail-open запись, get/list = чтения API) |
| `audit_records_dropped_total` | counter | аудит-записи, отброшенные при переполнении очереди async-записи |
| `unknown_format_passthrough_total` | counter | тела ответа пропущены без изменений из-за неизвестного API-формата (fail-open); ненулевой темп указывает на устаревший masking state (rolling upgrade) или несконфигурированный путь |
| `unguarded_path_passthrough_total` | counter | запросы пропущены без маскирования, потому что путь не совпал ни с одним охраняемым путём LLM (маскировать/пропускать — никогда не блокировать); ненулевой темп значит, что фильтр ext_proc навешен на маршруты, которые `GUARDRAILS_PATHS` не покрывает |

Плюс стандартные серверные gRPC-метрики из `go-grpc-prometheus`.

## Алертинг

Алертинг pull-based: Prometheus вычисляет правила по метрикам выше, Alertmanager
маршрутизирует уведомления. Поставляемые артефакты:

- `deploy/prometheus/guardrails-llm-filter-extproc-alerts.yml` — обычная группа правил
  Prometheus (добавьте в `rule_files`). Валидация после правок:
  `promtool check rules deploy/prometheus/guardrails-llm-filter-extproc-alerts.yml`.
- `deploy/kubernetes/components/monitoring/` — opt-in kustomize-компонент для
  prometheus-operator: `ServiceMonitor` (скрейпит порт `metrics` каждые 30s) +
  `PrometheusRule` с **той же группой — держите два файла синхронными**. Включение из
  overlay:

  ```yaml
  apiVersion: kustomize.config.k8s.io/v1beta1
  kind: Kustomization
  resources: [../../deploy/kubernetes]
  components: [../../deploy/kubernetes/components/monitoring]
  ```

Поставляемые алерты (подстройте пороги и матчер `job` под своё окружение):

| Алерт | Severity | Смысл |
|---|---|---|
| `GuardrailsMaskingFailures` | critical | ошибки маскирования → запросы ушли на upstream **немаскированными** (fail-open) |
| `GuardrailsDemaskFailures` | warning | клиенты могут видеть сырые плейсхолдеры |
| `GuardrailsStateStoreFailures` | warning | межрепличное демаскирование под угрозой |
| `GuardrailsAuditWriteFailures` / `GuardrailsAuditRecordsDropped` | warning | пробелы в аудит-трейле |
| `GuardrailsMaskedTrafficSpike` | warning | темп маскированных запросов 3× к тому же окну вчера — возможная утечка PII в промптах |
| `GuardrailsPipelineSlow` | warning | p99 mask+demask > 1s в течение 10m |
| `GuardrailsScrapeDown` | critical | нет метрик; позицию трафика решает Envoy `failure_mode_allow` |

### Дашборд Grafana

`deploy/grafana/dashboard.json` — импорт через *Dashboards → Import* (или provisioning);
при импорте спрашивает datasource Prometheus. Четыре ряда: трафик и детекции (темп
маскированных запросов по `mode` — вид shadow-раскатки, топ сработавших правил, разбивка
по типам данных), латентность (pipeline p50/p90/p99 с прорисованным порогом алерта 1s,
перцентили mask/scan/demask, объём скана), fail-open-ошибки (mask/demask/store/audit —
каждая панель указывает покрывающий её алерт) и здоровье gRPC/сервиса. Запросы панелей
переиспользуют выражения алертов, где они есть, но дашборд **не несёт алертов Grafana** —
две копии правил Prometheus остаются единым источником истины для алертинга.

## Аудит-трейл

`GUARDRAILS_AUDIT_ENABLED=true` пишет пофазовые метаданные маскирования (правила, типы
данных, плейсхолдеры — никогда оригиналы) в хранилище и открывает
`GET /v1/audit/records[?filters]` / `GET /v1/audit/records/{id}` на config API. Retention
`GUARDRAILS_AUDIT_RETENTION` (по умолчанию 24h); записи асинхронны и fail-open
(отбросы/сбои метрятся, см. алерты). `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS=true`
дополнительно хранит маскированные тексты промптов — см. чеклист ниже перед включением.

## Заголовки ответа

- `x-guardrails-data-types-triggered: 5,2` — когда маскирование сработало.
- `x-guardrails-triggered-rules: pii.email,...` — только с
  `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES=true`.

## Деплой

- **Docker**: корневой `Dockerfile` — multi-stage, builder `golang:1.26-alpine`, runtime
  `gcr.io/distroless/static-debian12:nonroot`, CGO off, копирует `configs/`. Без
  build-args, без приватных registry/netrc.
- **Kubernetes**: kustomize-база `deploy/kubernetes/` — deployment (gRPC liveness/readiness
  пробы на 9005, anti-affinity, envFrom configmap + secret), service, configmap
  (`GUARDRAILS_*`), secret (креды хранилища — config API неаутентифицирован и не требует
  своего секрета). Плейсхолдер образа
  `ghcr.io/cloud-ru-tech/guardrails-llm-filter-extproc:latest`. База **не включает
  edge-прокси**: вы должны поставить перед сервисом Envoy (или шлюз), который навешивает
  фильтр ext_proc и гигиену заголовков из чеклиста ниже — вырезает `x-guardrails-data-types`
  из клиентского трафика и генерирует `x-request-id` на сервере. Референс-конфиг —
  `examples/quickstart/proxy-config.yaml`.
- **Много реплик**: используйте хранилище `redis`/`postgres`, чтобы кастомные
  правила/настройки были общими, а masking state переживал приземление запроса и ответа
  на разные реплики; тикеры обновления (30s по умолчанию) сходят изменения политики.

## Чеклист безопасности для операторов

1. Вырезайте override-заголовок из клиентского трафика в Envoy
   (`request_headers_to_remove: [x-guardrails-data-types]`). Иначе подделанный
   `x-guardrails-data-types: none` отключит маскирование для этого запроса.
2. Генерируйте `x-request-id` на доверенном крае и никогда не сохраняйте клиентскую копию
   (Envoy `generate_request_id: true`, `preserve_external_request_id: false`). Он — ключ
   masking state; клиент, контролирующий его, в топологиях с общим хранилищем может читать
   или затирать чужие сохранённые оригиналы. Соль ключа состояния от этого не защищает —
   см. [configuration/settings.md](../configuration/settings.md).
3. Config API (REST :9080 и gRPC :9090), **включая `/v1/audit`, — административная
   плоскость** без аутентификации и без пообъектной авторизации, поэтому любой, кто до неё
   дотянется, может читать любое правило и любую аудит-запись. Защищайте на сетевом уровне:
   держите и :9080, и :9090 только внутри кластера, недоступными недоверенным сетям, и
   никогда не выставляйте через публичный ingress.
4. С хранилищем redis/postgres: ограничьте доступ — masking state держит исходные
   чувствительные значения (TTL'ятся, удаляются при закрытии потока).
5. Сознательно выбирайте позицию при отказе: сервис внутренне fail-open; Envoy
   `failure_mode_allow: false` делает отказ guardrails fail-closed.
6. Не гоняйте уровень лога `debug` в проде (на debug логируются заголовки запросов).
7. Аудит-записи содержат ID правил и плейсхолдеры (безопасны по построению — без
   оригиналов). `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS=true` — другой класс данных: он
   хранит **пользовательский контент промпта** (маскированный, но всё вокруг плейсхолдеров
   цело). Включайте только с хранилищем под контролем доступа, config API строго внутри
   кластера (никогда публичный ingress) и `GUARDRAILS_AUDIT_RETENTION`, настроенным под
   ваши требования комплаенса.

## Режимы отказа

| Отказ | Поведение |
|---|---|
| хранилище недоступно на старте | pod не стартует (Ping фабрики) — намеренно |
| хранилище недоступно в рантайме | data-path не затронут (in-process-состояние), мутации правил через API падают, reload держит последний снимок, счётчики тикают |
| файл правил невалиден на старте | паника — намеренно |
| невалидное кастомное правило в хранилище | пропускается, только если перекрывает builtin; иначе `Build` падает и продолжает работать предыдущий снимок |
| ошибка masker/demasker | пропуск маскированного/исходного контента, метрика + лог |
| сервис guardrails полностью недоступен | решает Envoy `failure_mode_allow` |
