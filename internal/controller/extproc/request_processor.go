package extproc

import (
	"context"
	"time"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/guardrails/demask"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/metrics"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/guardrails/mask"
)

// requestProcessor holds per-request state for the guardrails processing pipeline.
// One instance is created per Envoy stream (i.e. per HTTP request/response cycle).
type requestProcessor struct {
	Md           models.Metadata
	IsSse        bool
	Conf         *config.Config
	opts         []any
	skipStepMask ProcessStep

	SettingsProvider SettingsProvider
	Settings         models.EffectiveSettings
	PathResolver     *models.PathResolver

	MaskingState     models.MaskingState
	Masker           Masker
	StateStore       repository.MaskingStateStore
	Audit            AuditRecorder // nil when the audit trail is disabled
	DemaskerProvider DemaskerProvider
	DemaskerFactory  DemaskerFactory

	fullBodyBuf          []byte       // buffer for non-SSE body accumulation in FULL_DUPLEX_STREAMED mode
	sseProcessor         SseProcessor // SSE stream processor, created on first SSE body chunk
	maskedResponseTexts  []string     // captured pre-demask response text (non-SSE) for the audit trail

	// metrics
	guardActive      bool
	pipelineDuration time.Duration // masking + demaskig
}

// SettingsProvider supplies the current global guardrails settings.
type SettingsProvider interface {
	Global() models.GuardrailsSettings
}

// Masker masks request texts using guardrails regex rules.
type Masker interface {
	Handle(ctx context.Context, cmd mask.Command) (mask.CommandResponse, error)
}

// AuditRecorder persists per-request masking audit entries. Implementations
// must be non-blocking and fail-open (see internal/service/audit).
type AuditRecorder interface {
	Record(md models.Metadata, st models.MaskingState, maskedTexts []string)
	// RecordResponse enriches an existing record (by request ID) with the
	// masked model response texts; best-effort, gated, no-op if absent.
	RecordResponse(requestID string, maskedResponseTexts []string)
}

// DemaskerProvider creates request-scoped demasker factories from app-scoped dependencies.
type DemaskerProvider interface {
	NewFactory(state models.MaskingState) *demask.Factory
}

// DemaskerFactory creates request-scoped Demasker instances. JSONDemasker
// variants JSON-escape restored originals for use inside JSON string contexts
// (streamed tool-input fragments).
type DemaskerFactory interface {
	Demasker() *demask.Demasker
	JSONDemasker() *demask.Demasker
}

type SseProcessor interface {
	ProcessChunk(ctx context.Context, body []byte, endOfStream bool) ([]byte, error)
}

func newRequestProcessor(
	cfg *config.Config,
	masker Masker,
	settingsProvider SettingsProvider,
	demaskerProvider DemaskerProvider,
	stateStore repository.MaskingStateStore,
	audit AuditRecorder,
	pathResolver *models.PathResolver,
) requestProcessor {
	return requestProcessor{
		Conf:             cfg,
		Masker:           masker,
		DemaskerProvider: demaskerProvider,
		SettingsProvider: settingsProvider,
		StateStore:       stateStore,
		Audit:            audit,
		PathResolver:     pathResolver,
		// MaskingState will be initialized in Request phase
		// Demasker will be initialized in Response phase
	}
}

func (p *requestProcessor) AddLogOpt(key string, value any) {
	p.opts = append(p.opts, key, value)
}

func (p *requestProcessor) LogOpts() []any {
	return p.opts
}

func (p *requestProcessor) Clear() {
	*p = newRequestProcessor(
		p.Conf,
		p.Masker,
		p.SettingsProvider,
		p.DemaskerProvider,
		p.StateStore,
		p.Audit,
		p.PathResolver,
	)
}

func (p *requestProcessor) Close() error {
	if p.guardActive {
		metrics.ObservePipelineDuration(p.pipelineDuration)
	}

	// Best-effort cleanup of externally stored masking state. The stream
	// context is already canceled at close time, so use a short detached one.
	//
	// Skipped when StateDeleteOnClose is false: in a split request/response
	// deployment the response phase may run on another replica and still need
	// this state, so deleting it here could race that read. The MaskingTTL
	// reclaims the entry instead.
	if p.Conf.Guardrails.StateDeleteOnClose && len(p.MaskingState.Replacements) > 0 && p.Md.StateKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := p.StateStore.DeleteMaskingState(ctx, p.Md.StateKey); err != nil {
			metrics.IncMaskingStateStoreFailure("delete")
		}
	}

	p.Clear()
	return nil
}

type ProcessStep uint8

const (
	StepRequestHeaders ProcessStep = 1 << iota
	StepRequestBody
	StepRequestTrailers
	StepResponseHeaders
	StepResponseBody
	StepResponseTrailers
	StepAll ProcessStep = 0b1111111
)

func (p *requestProcessor) Skip(steps ...ProcessStep) {
	for _, stepMask := range steps {
		p.skipStepMask |= stepMask
	}
}

func (p *requestProcessor) ShouldSkip(step ProcessStep) bool {
	return p.skipStepMask&step > 0
}
