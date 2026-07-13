package extproc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/logging"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/metrics"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/sseproc"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/sseproc/common"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/extprocutils"
)

// HandleResponseHeaders processes HTTP response headers from Envoy.
// It determines if the response is a streaming SSE response and initialises
// a Demasker whenever masking is active.
func (p *requestProcessor) HandleResponseHeaders(ctx context.Context, req *extprocv3.HttpHeaders) (*extprocv3.ProcessingResponse, error) {
	// Convert Envoy headers to a map for easier processing
	headers := extprocutils.HeadersToMap(req.Headers)

	logging.Debug(ctx, "received ResponseHeaders message", append(p.LogOpts(), "headers", redactHeaders(headers))...)

	// Check if this is a Server-Sent Events (SSE) streaming response
	contentType, ok := headers[contentTypeHeader]
	if ok && strings.Contains(contentType, ContentTypeSse) {
		p.IsSse = true
	}

	// Update the metadata with streaming information
	p.Md.IsStreaming = p.IsSse

	p.AddLogOpt("sse", p.IsSse)
	logging.Debug(ctx, "processed ResponseHeaders message", p.LogOpts()...)

	if p.MaskingState.IsEmpty() {
		// The in-process state is empty. Another replica may have handled the
		// request phase, so consult the shared store before skipping. When the
		// request phase did not run on this replica, StateKey is unset — derive
		// it from the response's x-request-id (Envoy propagates it on both).
		if p.Md.StateKey == "" {
			if rid := headers[requestIDHeader]; rid != "" {
				p.Md.RequestID = rid
				p.Md.StateKey = deriveStateKey(p.Conf.Guardrails.StateKeySalt, rid)
			}
		}
		if st, err := p.StateStore.GetMaskingState(ctx, p.Md.StateKey); err == nil {
			p.MaskingState = st
			// Recover the wire format resolved by the request phase so SSE and
			// full-body dispatch pick the right processor instead of defaulting
			// to chat-completions.
			if p.Md.Format == "" {
				p.Md.Format = st.Format
			}
		} else if !errors.Is(err, repository.ErrNotFound) {
			op := "get"
			if errors.Is(err, repository.ErrUndecryptable) {
				op = "decrypt"
			}
			metrics.IncMaskingStateStoreFailure(op)
			logging.Warn(ctx, "Failed to read masking state from store",
				append(p.LogOpts(), "error", err)...)
		}
	}

	if p.MaskingState.IsEmpty() {
		logging.Debug(ctx, "No guardrails were triggered, skipping response", p.LogOpts()...)
		p.Skip(StepAll)
		return extprocutils.ModeOverrideSkipping(), nil
	}

	if len(p.MaskingState.TriggeredDataTypes) > 0 {
		dataTypes := make([]string, 0, len(p.MaskingState.TriggeredDataTypes))
		for _, dataType := range p.MaskingState.TriggeredDataTypes {
			dataTypes = append(dataTypes, strconv.Itoa(int(dataType)))
		}

		headers := []*corev3.HeaderValueOption{
			extprocutils.NewHeader(p.Conf.GuardrailsHeaders.DataTypesHeader, strings.Join(dataTypes, ",")),
		}

		if p.Conf.GuardrailsHeaders.ExposeTriggeredRules {
			headers = append(headers, extprocutils.NewHeader(p.Conf.GuardrailsHeaders.TriggeredRulesHeader, strings.Join(p.MaskingState.TriggeredRuleIDs, ",")))
		}

		return extprocutils.HeadersMutation(headers...), nil
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{},
		},
	}, nil
}

// HandleResponseBody demaskes synthetic tokens in LLM response bodies.
// Supports both non-streamed (full JSON) and SSE streaming responses.
func (p *requestProcessor) HandleResponseBody(ctx context.Context, body *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	logging.Debug(ctx, "HandleResponseBody", p.LogOpts()...)

	if len(p.MaskingState.Replacements) == 0 {
		return extprocutils.StreamedBodyMutation(body.GetBody(), body.GetEndOfStream()), nil // Do not change the body
	}

	// Initialize the DemaskerFactory for processing response body in both full and SSE modes
	if p.DemaskerFactory == nil {
		p.DemaskerFactory = p.DemaskerProvider.NewFactory(p.MaskingState)
	}

	demaskStartedAt := time.Now()
	defer func() {
		p.pipelineDuration += time.Since(demaskStartedAt)
	}()

	if !p.IsSse {
		return p.handleResponseBodyFull(ctx, body)
	}

	return p.handleResponseBodySse(ctx, body)
}

// handleResponseBodySse delegates SSE stream processing to the processor
// for the request's API format (Anthropic Messages, OpenAI Responses or
// OpenAI chat-completions).
func (p *requestProcessor) handleResponseBodySse(ctx context.Context, body *extprocv3.HttpBody) (*extprocv3.ProcessingResponse, error) {
	if p.sseProcessor == nil {
		p.sseProcessor = sseproc.NewForFormat(p.Md.Format,
			func() common.Demasker { return p.DemaskerFactory.Demasker() },
			func() common.Demasker { return p.DemaskerFactory.JSONDemasker() },
			p.Audit != nil)
	}

	bodyChunk := body.GetBody()
	eos := body.GetEndOfStream()

	startedAt := time.Now()
	defer func() {
		metrics.ObserveSSEChunkDemaskDuration(time.Since(startedAt))
	}()

	out, err := p.sseProcessor.ProcessChunk(ctx, bodyChunk, eos)
	if err != nil {
		metrics.IncDemaskSSEFailed()
		// The SSE processor uses internal fallbacks whenever possible, so an
		// error here is a real problem. Stay fail-open like the full-body path:
		// forward the chunk unchanged and keep the stream alive.
		logging.Warn(ctx, "SSE demask failed, passing chunk through unchanged (fail-open)",
			append(p.LogOpts(), "error", err)...)
		return extprocutils.StreamedBodyMutation(bodyChunk, eos), nil
	}

	// At end-of-stream, best-effort enrich the audit record with the masked
	// (pre-demask) response text the processor accumulated.
	if eos && p.Audit != nil {
		if src, ok := p.sseProcessor.(common.MaskedResponseTextSource); ok {
			if texts := src.MaskedResponseText(); len(texts) > 0 {
				p.Audit.RecordResponse(p.Md.RequestID, texts)
			}
		}
	}

	if out == nil && !eos {
		return nil, nil
	}

	if out == nil {
		out = []byte{}
	}

	return extprocutils.StreamedBodyMutation(out, eos), nil
}

// IMPORTANT: response_trailer_mode MUST be set to SEND
func (p *requestProcessor) HandleResponseTrailers(_ context.Context, _ *extprocv3.HttpTrailers) (*extprocv3.ProcessingResponse, error) {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extprocv3.TrailersResponse{},
		},
	}, nil
}
