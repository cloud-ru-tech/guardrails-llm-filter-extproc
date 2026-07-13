# Management API (HTTP + gRPC)

Контракт-первичный: management API определён proto-первично в
`api/proto/cloudru/guardrails/v1/service/**` (сервис `GuardrailsApi`) и генерируется
через `make gen-proto` в gRPC-сервер на `GUARDRAILS_API_GRPC_ADDR` (по умолчанию `:9090`)
плюс REST-прокси grpc-gateway перед ним, реализованный `internal/controller/api`, на
`GUARDRAILS_API_ADDR` (по умолчанию `:9080`, пусто отключает оба). Маршализатор
grpc-gateway использует `UseProtoNames`+`UseEnumNumbers`, поэтому REST-маршруты, имена
JSON-полей и формы ниже соответствуют контракту. Машиночитаемая спека:
`service.swagger.json` (объединённый OpenAPI v2, генерируется — **не править руками;
перегенерировать `make gen-proto` после изменения `.proto`**).

## Аутентификация

API **неаутентифицирован** — in-process-аутентификации нет ни на REST-порту, ни на
gRPC-порту. Защищайте на сетевом уровне: держите оба порта только внутри кластера и
никогда не выставляйте через публичный ingress.

> **Административная плоскость, без объектной авторизации.** Это однотенантный admin API
> без аутентификации и без пообъектной проверки. Любой, кто до него дотянется, может
> читать и менять **любое** правило и читать **любую** аудит-запись
> (`GET /v1/audit/records`) — включая маскированные тексты запроса при
> `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS=true`. Относитесь как к control plane: держите вне
> недоверенных сетей (только внутри кластера, никогда не публичный ingress) и оставляйте
> `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS` в дефолтном `false`, если хранилище не под
> контролем доступа. См. [operations/](../operations/).

## Эндпоинты

| Метод и путь | Поведение |
|---|---|
| `GET /v1/rules?source=all\|builtin\|custom` | список правил; неверный source → 400 |
| `POST /v1/rules` | создать кастомное правило → 201; валидация → 400; дубль id (custom **или builtin**) → 409 |
| `GET /v1/rules/{id}` | builtin или custom → 200; нет → 404 |
| `PUT /v1/rules/{id}` | заменить кастомное правило → 200; rule_id в теле должен совпасть с путём (или быть пустым) → 400; builtin → 409; нет → 404 |
| `PATCH /v1/rules/{id}` | включить/выключить правило (`{"enabled": false}`) — **единственная мутация, разрешённая над builtin** → 200 с правилом; нет `enabled`/неизвестное поле → 400; нет правила → 404. Идемпотентно |
| `DELETE /v1/rules/{id}` | → 204; builtin → 409; нет → 404 (также снимает флаг disabled) |
| `GET /v1/settings` | текущие глобальные настройки (всегда с `mode`) |
| `PUT /v1/settings` | заменить настройки → 200; неизвестный тип данных или mode → 400. `mode` опционален: пропущен → `enforce` (PUT заменяет весь документ) |
| `GET /v1/data-types` | группы из YAML-файлов + синтетическая запись CUSTOM |
| `GET /v1/audit/records` | список аудит-записей маскирования (фильтры + курсорная пагинация); **регистрируется только при `GUARDRAILS_AUDIT_ENABLED=true`**, иначе 404 |
| `GET /v1/audit/records/{request_id}` | аудит-запись одного запроса; нет/истёк → 404 |

Единый формат ошибки: `{"error": "...", "details": "..."}`.

## DTO (`dto.go`)

`RuleDTO` повторяет `rule.Rule`, но это отдельный тип — wire-контракт не должен дрейфовать
с внутренностями движка. `source: builtin|custom` и `enabled` — только для ответа
(`enabled` отражает сохранённый disabled-набор, а не поле правила; на входе принимается,
но игнорируется, чтобы ответы GET можно было переслать обратно). `dto.toRule()` ставит
`DefaultOn: true` для правил из API.

Отключённые правила остаются в списке (с `enabled: false`), но исключаются из
скомпилированного снимка реестра, поэтому сканер их не запускает; другие реплики сходятся
через тикер `GUARDRAILS_RULES_REFRESH_INTERVAL`.

`SettingsDTO.data_types` принимает **числа или имена** на входе
(`DataTypeValue.UnmarshalJSON` пробует сначала JSON-число, затем имя через
`settings.ParseDataTypes`) и всегда отдаёт числа.

Тела запросов: `MaxBytesReader` на 1 MiB + `DisallowUnknownFields`.

## Аудит-эндпоинты

Обслуживаются `audit.go`; зависимость `AuditService` — само хранилище (набор методов =
read-сторона `repository.AuditStore`), `nil` при выключенном аудите — тогда маршруты
просто не регистрируются.

- **Query-параметры списка**: `model`, `path`, `rule_id` (точный/по вхождению фильтры),
  `data_type` (число или имя, переиспользует парсер настроек), `since`/`until` (RFC3339),
  `limit` (по умолчанию 50, максимум 500), `cursor`.
- **Пагинация**: keyset по `(timestamp desc, request_id desc)`; ответ несёт
  `next_cursor`, пока есть страницы. С redis-бэкендом сильно отфильтрованный запрос может
  вернуть короткую страницу **с** курсором — листайте, пока `next_cursor` не исчезнет.
- **Ошибки**: битый курсор / битые параметры → 400, неизвестный request_id → 404, сбой
  хранилища → 500 (+ `audit_store_failures_total{op=get|list}`).
- Записи никогда не содержат оригиналы; `masked_texts` появляется только при
  `GUARDRAILS_AUDIT_STORE_MASKED_TEXTS=true`.

```sh
curl ':9080/v1/audit/records?data_type=personal_data&limit=20'
curl ':9080/v1/audit/records/<x-request-id>'
```

## Поток мутации правила

Хендлеры делегируют per-scenario-хендлерам `internal/usecases/rules` (см.
[rules-engine/custom-rules.md](../rules-engine/custom-rules.md)): валидация происходит
**до** записи (`ruleIDRe`, DataType 1–6, `CompileRule` — тот самый продакшн-путь
компиляции, поэтому текст детали 400 — реальная ошибка компиляции), затем запись в
хранилище, затем пересборка + атомарная замена реестра. Правило, созданное через API,
активно на следующем запросе **без рестарта**; другие реплики подхватывают его в пределах
`GUARDRAILS_RULES_REFRESH_INTERVAL`.

Маппинг ошибок в `writeRuleMutationError` (`rules.go`): `*rules.ValidationError` → 400,
`rules.ErrNotFound` → 404, `rules.ErrAlreadyExists` / `rules.ErrBuiltin` → 409, остальное
→ 500.

## Пример сессии

```sh
# добавить правило (data_type 6 = CUSTOM)
curl -X POST :9080/v1/rules -d '{
  "rule_id":"acme_token","name":"ACME token","data_type":6,
  "regex":"\\bacme-[0-9a-f]{8}\\b","masking":{"placeholder":"ACME_TOKEN"}}'

# CUSTOM (6) должен быть во включённых типах данных, чтобы такие правила работали. Он есть
# в поставляемом дефолте; нужно только если вы сузили data_types и убрали его.
curl -X PUT :9080/v1/settings -d '{"enabled":true,"data_types":[1,2,3,4,5,"custom"]}'
```

После этих двух вызовов следующий запрос через Envoy маскирует `acme-deadbeef` в
`<ACME_TOKEN_1>` на upstream и демаскирует обратно в ответе клиенту.
