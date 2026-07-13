# Жизненный цикл запроса (поток ext_proc)

Один цикл запрос/ответ Envoy = один двунаправленный gRPC-поток = один экземпляр
`requestProcessor` (`internal/controller/extproc/request_processor.go`).
`Controller.Process` крутит цикл на `srv.Recv()` и диспетчеризует каждый вариант
`ProcessingRequest_*` через `handleRequest`, учитывая пофазовую **битовую маску
пропуска** (`ProcessStep`, `Skip(StepAll)` закрывает поток раньше) и
`ObservabilityMode` (ответы не отправляются).

## Требуемая конфигурация фильтра Envoy

```yaml
processing_mode:
  request_header_mode: SEND
  response_header_mode: SEND
  request_body_mode: BUFFERED              # запрос маскируется целиком
  response_body_mode: FULL_DUPLEX_STREAMED # ответ демаскируется потоком
  response_trailer_mode: SEND              # ОБЯЗАТЕЛЬНО для full-duplex тел
failure_mode_allow: true                   # выбор позиции отказоустойчивости оператором
```

**Почему асимметрично:** `HandleRequestBody` ожидает полное тело одним сообщением и
отвечает `extprocutils.BodyMutation` (`BodyMutation_Body` + заголовок content-length)
— контракт BUFFERED. Сторона ответа отвечает `extprocutils.StreamedBodyMutation`
(`BodyMutation_StreamedResponse`) — контракт FULL_DUPLEX. Смешение режимов ломает
трафик (наблюдаемый симптом: upstream получает пустое тело / «EOF»). Референс-конфиг:
`examples/quickstart/proxy-config.yaml`.

Дополнительно на маршруте: `request_headers_to_remove: [x-guardrails-data-types]` —
см. [configuration/settings.md](../configuration/settings.md) (модель доверия).

## Фаза 1 — RequestHeaders (`process_request.go`)

1. Путь резолвится в API-формат (`models.PathResolver`, построен из
   `GUARDRAILS_PATHS`; по умолчанию `/v1/chat/completions`, `/v1/messages`,
   `/v1/responses`). Матчинг обрезает query-строку, пробует точное совпадение,
   затем самый длинный сконфигурированный суффикс — поэтому проксирующие
   монтирования вроде `/openai/v1/chat/completions` резолвятся без настройки.
   Формат попадает в `Metadata.Format` и определяет всю дальнейшую диспетчеризацию
   (извлечение, SSE-процессор, демаскирование полного тела). Нерезолвимый путь →
   немедленный ответ 400 (`errors.go`) + skip all. Это единственное место, где
   сервис отвечает вместо upstream.
2. `RequestID` = заголовок `x-request-id`, fallback `uuid.NewString()` (fallback
   корректен внутри потока, но не находится между репликами — это нормально, там и
   нечего искать).
3. `Model` = `x-gateway-model-name` (только для логов), лежит в `models.Metadata`.
4. `p.Settings = settings.Effective(SettingsProvider.Global(), overrideHeaderValue)`
   — чистое in-memory-вычисление, **сетевого I/O в этой фазе нет**.
5. Отключённые или пустые типы данных → `Skip(StepAll)` +
   `extprocutils.ModeOverrideSkipping()` (просит Envoy не слать оставшиеся фазы).

## Фаза 2 — RequestBody

1. Пустое тело / нет извлекаемых полей → skip all, пропуск.
2. Извлечение по формату: `chatcompletions.ExtractRequestContent` берёт
   `messages[].content` (строка или текстовые части), `tool_calls[].function.arguments`,
   `function_call.arguments`, эквиваленты Anthropic. Тело без массива `messages`
   (например, legacy `/v1/completions` с `prompt` — не поддерживается) даёт
   `ErrUnsupportedBodySchema` и пропускается без маскирования (fail-open), увеличивая
   `unsupported_body_schema_total`. Для `Format == responses`
   `responses.ExtractRequestContent` берёт `instructions`, `input` (строка или
   элементы: части `input_text`, `function_call_output.output`). У каждого поля есть
   gjson-путь для обратного патча.
3. `Masker.Handle(mask.Command{DataTypes, Texts})`: типы данных → ID правил
   (`Registry.GetRuleIDsByDataTypes`) → параллельный скан → дедуплицированные
   оригиналы получают инкрементные плейсхолдеры (`<TYPE_N>` через `placeholderfmt`) →
   возвращает маскированные тексты + `models.MaskingState` (`TriggeredRuleIDs`,
   `TriggeredDataTypes`, `Replacements`). Ошибка маскера → метрика + лог + skip all
   (fail-open).
4. Нет замен → skip all (позже нечего демаскировать).
5. **Detect (shadow) режим** (`Settings.Mode == detect`): пишет
   `requests_masked_total{mode="detect"}` + аудит-запись, затем skip all и пропускает
   тело без изменений — нет in-process `MaskingState`, нет записи в хранилище, так что
   фаза ответа (и очистка в `Close`) естественно ничего не делает. Шаги 6–7 — только
   для enforce.
6. `StateStore.PutMaskingState(ctx, RequestID, state)` — best-effort; сбой →
   `masking_state_store_failures_total{op="put"}` + warn, in-process-копия всё равно
   обслуживает этот поток.
7. Патч каждого изменённого поля через `sjson.SetBytes`, ответ `BodyMutation`
   (буферизованный).

Здесь наблюдаются метрики: длительность маскирования, количество сработавших правил,
счётчики по правилам и типам данных (растут в обоих режимах);
`requests_masked_total{mode}`; начинается накопление `pipelineDuration`.

## Фаза 3 — ResponseHeaders (`process_response.go`)

1. Определение SSE по `content-type: text/event-stream` → `p.IsSse`.
2. Если in-process `MaskingState` пуст, пробуем `StateStore.GetMaskingState(RequestID)`
   — межрепличный fallback (фазу запроса мог обработать другой экземпляр).
   `ErrNotFound` — норма; прочие ошибки → метрика + warn.
3. Всё ещё пусто → полностью пропустить обработку ответа.
4. Иначе выдать заголовки ответа: `x-guardrails-data-types-triggered: 5,2` всегда;
   `x-guardrails-triggered-rules: pii.email,...` только при
   `GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES=true` (ID правил раскрывают детекторы →
   opt-in).

## Фаза 4 — ResponseBody

`DemaskerFactory` создаётся лениво из `DemaskerProvider.NewFactory(MaskingState)`.

- **Non-SSE** (`process_response_body_full.go`): под FULL_DUPLEX Envoy всё равно
  отдаёт тело кусками — буферизуем в `fullBodyBuf` до `end_of_stream`, затем
  демаскируем полный JSON, диспетчеризация по `Metadata.Format`. Путь OpenAI
  chat/completions разбирает `chatcompletions.Response` и демаскирует
  content/reasoning/аргументы tool-call по каждому choice; путь Anthropic
  (`anthropic.Message`) демаскирует блоки `text`/`thinking`/`tool_use.input`
  (пропускает `redacted_thinking`); путь OpenAI Responses патчит на месте через
  gjson/sjson (`responses.ExtractOutputFields`: тексты `output_text`,
  `function_call.arguments`, `summary_text` в reasoning) — без типизированной
  структуры, потому что объект Responses быстро эволюционирует и типизированный
  round-trip терял бы неизвестные поля. Аргументы tool-call требуют
  decode→demask→re-encode + защиту `json.Valid` (сломанное демаскирование → оставить
  маскированное значение). Финальный marshal через `marshalNoEscape` (без
  HTML-экранирования — плейсхолдеры вроде `<EMAIL_1>` должны сохраниться байт-в-байт);
  путь sjson не перемаршаливает. Сбои unmarshal/marshal → вернуть исходное тело.
- **SSE** (`internal/sseproc`): `NewForFormat` выбирает процессор chatcompletions,
  messages или responses. Они парсят кадры `data:`, гоняют дельты через
  per-field-демаскеры, которые **буферизуют суффикс** длиной с максимально возможный
  плейсхолдер (из `GetMaxPlaceholderLenByRuleIDs`, с допуском на дрейф), чтобы
  плейсхолдеры, разбитые между кусками, всё равно демаскировались; UTF-8-безопасно.
  Выходные куски могут быть пере-сегментированы относительно входных — это ожидаемо.
  Процессор responses дополнительно демаскирует полный текст, который события
  Responses повторяют (`output_text.done`, `output_item.done`, объект ответа в
  `response.completed`), свежими одноразовыми демаскерами — иначе плейсхолдеры утекли
  бы в финальный снимок. Ошибки внутри процессора предпочитают маскированные fallback'и
  падению; возвращённая ошибка — реальная проблема и обрывает поток.

## Фаза 5 — Trailers и Close

Trailers эхом отдаются как есть. `requestProcessor.Close()` (deferred в `Process`):
наблюдает `pipeline_duration_seconds`, затем best-effort `DeleteMaskingState` с
**отдельным контекстом на 2 с** (контекст потока к этому моменту уже отменён), затем
`Clear()`.

## Семантика пропуска

`Skip(StepAll)` и выставляет битовую маску, и — через `ModeOverrideSkipping()` —
отправляет Envoy `ModeOverride`, отключающий оставшиеся фазы, так что отключённый
запрос стоит одного обмена заголовками. Когда выставлен `ShouldSkip(StepAll)`,
контроллер выходит из `Process` (закрывает поток ext_proc).
