package extproc

import (
	"context"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/logging"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/metrics"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/service/settings"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/guardrails/mask"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/extprocutils"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/chatcompletions"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/messages"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/responses"
)

func (p *requestProcessor) HandleRequestHeaders(ctx context.Context, req *extprocv3.HttpHeaders) (*extprocv3.ProcessingResponse, error) {
	// Convert Envoy headers to a map for easier processing
	headers := extprocutils.HeadersToMap(req.Headers)

	logging.Debug(ctx, "Received RequestHeaders message", "headers", redactHeaders(headers))

	// Extract the request path to determine if we should process this request
	path := headers[pathHeader]
	p.AddLogOpt("path", path)

	// Resolve the path to its API format (exact match, then longest suffix
	// so proxy-prefixed mounts work). An unresolvable path is not one we
	// guard, so pass it through untouched — the service masks or passes,
	// never blocks (mask/pass — never block).
	format, ok := p.PathResolver.Resolve(path)
	if !ok {
		// Unmatched paths pass through unmasked (mask/pass — never block); the
		// counter is the only signal an operator gets that unguarded traffic
		// transits the filter.
		metrics.IncUnguardedPathPassthrough()
		logging.Debug(ctx, "Unsupported path, passing request through unchanged", p.LogOpts()...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	// The request ID tags logs and identifies the audit record. Envoy normally
	// supplies it; the fallback UUID is stream-local-correct. The masking-state
	// store key is derived from it (not the raw value) — see deriveStateKey.
	requestID := headers[requestIDHeader]
	if requestID == "" {
		requestID = uuid.NewString()
	}
	p.AddLogOpt("request_id", requestID)

	// Extract the model name from headers if available (log-only).
	model := headers[modelNameHeader]
	if model != "" {
		p.AddLogOpt("model", model)
	}

	// Populate the metadata struct with extracted information
	p.Md = models.Metadata{
		RequestID: requestID,
		StateKey:  deriveStateKey(p.Conf.Guardrails.StateKeySalt, requestID),
		Model:     model,
		Path:      path,
		Format:    format,
	}

	// Resolve the effective settings: global policy, optionally narrowed by
	// the trusted gateway override header. No network I/O happens here.
	var overrideValue string
	if p.Conf.Guardrails.OverrideHeader != "" {
		overrideValue = headers[p.Conf.Guardrails.OverrideHeader]
	}
	p.Settings = settings.Effective(p.SettingsProvider.Global(), overrideValue)
	p.Md.Mode = p.Settings.Mode
	p.AddLogOpt("mode", string(p.Settings.Mode))

	if !p.Settings.Enabled || len(p.Settings.DataTypes) == 0 {
		logging.Debug(ctx, "Guardrails are disabled for this request, skipping", p.LogOpts()...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	logging.Debug(ctx, "Processed RequestHeaders message")

	// Return an empty response to continue processing without modifying the request
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
	}, nil
}

// HandleRequestBody scans the request body for PII/secrets and masks them.
// The body is expected to be a JSON OpenAI-compatible request.
// On guardrails disabled the body is passed through unchanged.
func (p *requestProcessor) HandleRequestBody(ctx context.Context, body *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	logging.Debug(ctx, "HandleRequestBody", p.LogOpts()...)

	if !p.Settings.Enabled {
		logging.Debug(ctx, "guardrails are disabled for this request, overriding skipping", "model", p.Md.Model)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	bodyBytes := body.GetBody()
	if len(bodyBytes) == 0 {
		logging.Debug(ctx, "no body found, overriding skipping")
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	// The model name lives in the request body (top-level "model" field) for
	// every supported format. Prefer it over the x-gateway-model-name header
	// (only present when injected upstream), so the audit record captures it.
	if model := gjson.GetBytes(bodyBytes, "model").String(); model != "" {
		p.Md.Model = model
		p.AddLogOpt("model", model)
	}

	maskStartedAt := time.Now()
	defer func() {
		p.guardActive = true
		p.pipelineDuration = time.Since(maskStartedAt)
		metrics.ObserveMaskDuration(time.Since(maskStartedAt))
	}()

	// Extract scannable request payload fields per API format:
	// chat/completions — messages[].content, content text parts,
	// tool_calls[].function.arguments, function_call.arguments;
	// responses — instructions, input (string/items), function_call_output;
	// messages (Anthropic) — messages[].content text, tool_use.input,
	// tool_result.content.
	var fields []llmutils.ContentField
	var err error
	switch p.Md.Format {
	case models.APIFormatResponses:
		fields, err = responses.ExtractRequestContent(bodyBytes)
	case models.APIFormatMessages:
		fields, err = messages.ExtractRequestContent(bodyBytes)
	default:
		fields, err = chatcompletions.ExtractRequestContent(bodyBytes)
	}
	if err != nil {
		// The path is guarded but the resolved format could not parse this body
		// (e.g. a legacy /v1/completions prompt-body mapped to chat_completions,
		// which is unsupported). Fail open, but surface it via a counter — unlike
		// a benign empty body, this usually signals a GUARDRAILS_PATHS misconfig.
		metrics.IncUnsupportedBodySchema()
		logging.Debug(ctx, "unsupported request body schema for format, passing through unchanged", p.LogOpts()...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}
	if len(fields) == 0 {
		logging.Debug(ctx, "no fields found, overriding skipping")
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	texts := make([]string, len(fields))
	for i, f := range fields {
		texts[i] = f.Value
	}

	result, err := p.Masker.Handle(ctx, mask.Command{
		DataTypes: p.Settings.DataTypes,
		Texts:     texts,
	})
	if err != nil {
		metrics.IncMaskFailed()
		logging.Error(ctx, "Mask use case error, pass-through", err, p.LogOpts()...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	metrics.ObserveTriggeredRules(len(result.MaskingState.TriggeredRuleIDs))
	for _, ruleID := range result.MaskingState.TriggeredRuleIDs {
		metrics.IncRuleTrigger(ruleID)
	}
	for _, dataType := range result.MaskingState.TriggeredDataTypes {
		metrics.IncDataTypeTrigger(dataType.String())
	}

	if len(result.MaskingState.Replacements) == 0 {
		logging.Debug(ctx, "no replacements found, passing through unchanged")
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	// Detect (shadow) mode: record what would have been masked, but pass
	// the request through unchanged. No in-process or persisted masking
	// state means the response phase naturally skips demasking too.
	if p.Settings.Mode == models.ModeDetect {
		metrics.IncRequestMasked(string(models.ModeDetect))
		if p.Audit != nil {
			p.Audit.Record(p.Md, result.MaskingState, result.MaskedTexts)
		}
		logging.Debug(ctx, "Detect mode: sensitive data found, passing through unchanged",
			append(p.LogOpts(), "rules", result.MaskingState.TriggeredRuleIDs)...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	// Record the resolved wire format so a response-only replica reading this
	// state from the store can pick the correct demask/SSE processor.
	result.MaskingState.Format = p.Md.Format

	// Patch before recording metrics/state/audit so telemetry only ever claims
	// masking that was actually applied to the outgoing body. Every extracted
	// field is a decoded string value (tool_use.input is extracted per string
	// leaf), so a string set on its pre-existing path cannot break the JSON.
	patched := bodyBytes
	for i, f := range fields {
		maskedText := result.MaskedTexts[i]
		if maskedText == f.Value {
			continue
		}
		var patchErr error
		patched, patchErr = sjson.SetBytes(patched, f.Path, maskedText)
		if patchErr != nil {
			logging.Warn(ctx, "Failed to patch field", "path", f.Path, "error", patchErr)
		}
	}

	p.MaskingState = result.MaskingState
	metrics.IncRequestMasked(string(models.ModeEnforce))

	// Persist the state so another replica can demask the response when the
	// deployment splits request/response processing. Fail-open: neither a
	// store error nor a slow/hung store may block the request, so the write
	// gets its own bounded timeout detached from the stream context. The
	// detach matters: an Envoy stream cancellation (client disconnect) must
	// not abort the persist before its budget, otherwise a response-only
	// replica would miss the state and leak placeholders undemasked.
	// context.WithoutCancel keeps the request's values (trace/log correlation)
	// while dropping cancellation propagation.
	putCtx, cancelPut := context.WithTimeout(context.WithoutCancel(ctx), stateStorePutTimeout)
	if putErr := p.StateStore.PutMaskingState(putCtx, p.Md.StateKey, result.MaskingState); putErr != nil {
		metrics.IncMaskingStateStoreFailure("put")
		logging.Warn(ctx, "Failed to persist masking state, continuing with in-process state",
			append(p.LogOpts(), "error", putErr)...)
	}
	cancelPut()

	// Audit trail (optional): non-blocking, fail-open by contract.
	if p.Audit != nil {
		p.Audit.Record(p.Md, result.MaskingState, result.MaskedTexts)
	}

	response := extprocutils.BodyMutation(patched)

	logging.Debug(ctx, "Request body masked",
		append(p.LogOpts(), "rules", result.MaskingState.TriggeredRuleIDs)...)

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: response,
			},
		},
	}, nil
}

func (p *requestProcessor) HandleRequestTrailers(_ context.Context, _ *extprocv3.HttpTrailers) (*extprocv3.ProcessingResponse, error) {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extprocv3.TrailersResponse{},
		},
	}, nil
}
