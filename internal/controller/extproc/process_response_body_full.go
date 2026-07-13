package extproc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/logging"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/metrics"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/sseproc/common"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/extprocutils"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/chatcompletions"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/messages"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/llmutils/responses"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/tidwall/sjson"
)

// handleResponseBodyFull handles non-SSE responses.
// Because Envoy sends the body in chunks even for non-streamed responses
// (FULL_DUPLEX_STREAMED mode), we buffer all chunks until end_of_stream,
// then demask the complete JSON and send it in the final response.
func (p *requestProcessor) handleResponseBodyFull(ctx context.Context, body *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	p.fullBodyBuf = append(p.fullBodyBuf, body.GetBody()...)
	if !body.GetEndOfStream() {
		// Not yet the final chunk — buffer and do not send anything
		logging.Debug(ctx, "Skipping chunk, not EoS yet", p.LogOpts()...)
		return nil, nil
	}

	startedAt := time.Now()
	defer func() {
		metrics.ObserveDemaskDuration(time.Since(startedAt))
	}()

	// We have the complete body. Clear the buffer
	fullBody := p.fullBodyBuf
	p.fullBodyBuf = nil

	if len(fullBody) == 0 {
		return extprocutils.StreamedBodyMutation(fullBody, true), nil
	}

	// The handlers demask in place and always fall back to the original bytes
	// on any internal failure (fail-open), so they never return an error.
	var patchedBody []byte
	switch p.Md.Format {
	case models.APIFormatChatCompletions:
		patchedBody = p.handleOpenAICompletion(ctx, fullBody)
	case models.APIFormatResponses:
		patchedBody = p.handleOpenAIResponses(ctx, fullBody)
	case models.APIFormatMessages:
		patchedBody = p.handleAnthropicMessage(ctx, fullBody)
	default:
		// Unknown/empty format (rolling upgrade with pre-Format state, or a
		// future dialect): pass the body through untouched rather than risk
		// corrupting it. Fail-open by design.
		logging.Warn(ctx, "Unknown response format, passing body through unchanged",
			append(p.LogOpts(), "format", string(p.Md.Format))...)
		metrics.IncUnknownFormatPassthrough()
		patchedBody = fullBody
	}

	// Best-effort: enrich the audit record with the masked response text.
	if p.Audit != nil && len(p.maskedResponseTexts) > 0 {
		p.Audit.RecordResponse(p.Md.RequestID, p.maskedResponseTexts)
	}

	return extprocutils.StreamedBodyMutation(patchedBody, true), nil
}

// newDemasker returns a fresh request-scoped Demasker as a common.Demasker,
// suitable for passing to shared helpers such as common.DemaskJSONArguments.
func (p *requestProcessor) newDemasker() common.Demasker {
	return p.DemaskerFactory.Demasker()
}

// demaskToolCallArguments demasks the Function.Arguments field of a single
// tool call.
//
// The OpenAI wire format encodes tool-call arguments as a JSON string whose
// *content* is itself a JSON object, e.g. "arguments": "{\"path\": \"foo\"}".
// Demasking must keep that content valid JSON even when a restored original
// contains a quote/backslash/control char. common.DemaskJSONArguments is the
// single source of truth shared with the Responses SSE processor: it tries a
// fast raw substitution and falls back to a structural per-leaf demask when the
// naive result is invalid JSON.
func (p *requestProcessor) demaskToolCallArguments(ctx context.Context, arguments string) (string, error) {
	if len(arguments) == 0 {
		logging.Debug(ctx, "Empty arguments, no demasking needed", p.LogOpts()...)
		return "", nil
	}

	demasked, ok := common.DemaskJSONArguments(ctx, p.newDemasker, arguments)
	if !ok {
		return "", fmt.Errorf("demask tool call arguments failed")
	}
	return demasked, nil
}

// demaskAndPatchFields demasks each extracted field and patches it back in
// place with sjson, preserving every other byte of the body verbatim (no typed
// round-trip: a partial unmarshal/marshal would silently drop unmodeled fields
// and inject zero-valued ones; sjson does not HTML-escape). Fields whose path
// ends in ".arguments" (a JSON string containing an object) or ".input" (a raw
// JSON object) are demasked structurally via demaskToolCallArguments; ".input"
// fields are patched raw so they stay objects. Fail-open per field: on demask
// error the masked value is kept, on patch error the field is skipped.
func (p *requestProcessor) demaskAndPatchFields(ctx context.Context, body []byte, fields []llmutils.ContentField, what string) []byte {
	captureMasked := p.Audit != nil
	patched := body
	for _, f := range fields {
		isRawObject := strings.HasSuffix(f.Path, ".input")
		// Capture the model's masked text output for the audit trail. Structured
		// tool-call payloads (.arguments/.input) are excluded so the recorded
		// value is text, matching the SSE processors' capture.
		if captureMasked && !isRawObject && !strings.HasSuffix(f.Path, ".arguments") && f.Value != "" {
			p.maskedResponseTexts = append(p.maskedResponseTexts, f.Value)
		}
		var demasked string
		var err error
		if isRawObject || strings.HasSuffix(f.Path, ".arguments") {
			demasked, err = p.demaskToolCallArguments(ctx, f.Value)
		} else {
			demasked, err = p.DemaskerFactory.Demasker().DemaskChunk(ctx, f.Value, true)
		}
		if err != nil {
			metrics.IncDemaskFullFailed()
			logging.Error(ctx, "Failed to demask "+what+" field, keeping masked value", err,
				append(p.LogOpts(), "path", f.Path)...)
			continue
		}
		if demasked == f.Value {
			continue
		}
		var patchErr error
		if isRawObject {
			patched, patchErr = sjson.SetRawBytes(patched, f.Path, []byte(demasked))
		} else {
			patched, patchErr = sjson.SetBytes(patched, f.Path, demasked)
		}
		if patchErr != nil {
			logging.Error(ctx, "Failed to patch "+what+" field", patchErr,
				append(p.LogOpts(), "path", f.Path)...)
		}
	}
	return patched
}

// handleOpenAICompletion processes an OpenAI chat/completions format response
// body (see chatcompletions.ExtractOutputFields for the field set).
func (p *requestProcessor) handleOpenAICompletion(ctx context.Context, fullBody []byte) []byte {
	return p.demaskAndPatchFields(ctx, fullBody, chatcompletions.ExtractOutputFields(fullBody), "chat/completion")
}

// handleOpenAIResponses processes an OpenAI Responses API (/v1/responses)
// format response body (see responses.ExtractOutputFields).
func (p *requestProcessor) handleOpenAIResponses(ctx context.Context, fullBody []byte) []byte {
	return p.demaskAndPatchFields(ctx, fullBody, responses.ExtractOutputFields(fullBody, ""), "responses")
}

// handleAnthropicMessage processes an Anthropic Messages API format response
// body (see messages.ExtractResponseFields for the field set and the
// rationale for not round-tripping through the SDK typed Message).
func (p *requestProcessor) handleAnthropicMessage(ctx context.Context, fullBody []byte) []byte {
	return p.demaskAndPatchFields(ctx, fullBody, messages.ExtractResponseFields(fullBody), "messages")
}
