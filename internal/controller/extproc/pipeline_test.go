package extproc

import (
	"context"
	"errors"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/guardrails/mask"
)

// --- fakes ---

type fakeSettings struct{ global models.GuardrailsSettings }

func (f *fakeSettings) Global() models.GuardrailsSettings { return f.global }

type fakeMasker struct {
	gotDataTypes []models.DataType
	resp         mask.CommandResponse
	err          error
}

func (f *fakeMasker) Handle(_ context.Context, cmd mask.Command) (mask.CommandResponse, error) {
	f.gotDataTypes = cmd.DataTypes
	if f.err != nil {
		return mask.CommandResponse{}, f.err
	}
	if len(f.resp.MaskedTexts) == 0 {
		f.resp.MaskedTexts = cmd.Texts
	}
	return f.resp, nil
}

type fakeStateStore struct {
	putErr    error
	getErr    error
	entries   map[string]models.MaskingState
	puts      int
	deletes   int
	putCtxErr error // ctx.Err() observed inside the most recent PutMaskingState
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{entries: map[string]models.MaskingState{}}
}

func (f *fakeStateStore) PutMaskingState(ctx context.Context, id string, st models.MaskingState) error {
	f.puts++
	f.putCtxErr = ctx.Err()
	if f.putErr != nil {
		return f.putErr
	}
	f.entries[id] = st
	return nil
}

func (f *fakeStateStore) GetMaskingState(_ context.Context, id string) (models.MaskingState, error) {
	if f.getErr != nil {
		return models.MaskingState{}, f.getErr
	}
	st, ok := f.entries[id]
	if !ok {
		return models.MaskingState{}, repository.ErrNotFound
	}
	return st, nil
}

func (f *fakeStateStore) DeleteMaskingState(_ context.Context, id string) error {
	f.deletes++
	delete(f.entries, id)
	return nil
}

type auditCall struct {
	md          models.Metadata
	st          models.MaskingState
	maskedTexts []string
}

type fakeAuditRecorder struct {
	calls         []auditCall
	responseTexts map[string][]string
}

func (f *fakeAuditRecorder) Record(md models.Metadata, st models.MaskingState, maskedTexts []string) {
	f.calls = append(f.calls, auditCall{md: md, st: st, maskedTexts: maskedTexts})
}

func (f *fakeAuditRecorder) RecordResponse(requestID string, maskedResponseTexts []string) {
	if f.responseTexts == nil {
		f.responseTexts = make(map[string][]string)
	}
	f.responseTexts[requestID] = maskedResponseTexts
}

// fakeSseProcessor always fails, to exercise the SSE-error handling modes.
type fakeSseProcessor struct{ err error }

func (f *fakeSseProcessor) ProcessChunk(_ context.Context, _ []byte, _ bool) ([]byte, error) {
	return nil, f.err
}

// --- helpers ---

func testConfig() *config.Config {
	return &config.Config{
		Guardrails: config.Guardrails{
			OverrideHeader:     "x-guardrails-data-types",
			StateDeleteOnClose: true,
		},
		GuardrailsHeaders: config.GuardrailsHeaders{
			DataTypesHeader:      "x-guardrails-data-types-triggered",
			TriggeredRulesHeader: "x-guardrails-triggered-rules",
		},
	}
}

func allEnabled() models.GuardrailsSettings {
	return models.GuardrailsSettings{
		Enabled: true,
		DataTypes: []models.DataType{
			models.DataTypeCREDENTIALS,
			models.DataTypePERSONALDATA,
		},
	}
}

func requestHeaders(kv map[string]string) *extprocv3.HttpHeaders {
	headers := make([]*corev3.HeaderValue, 0, len(kv))
	for k, v := range kv {
		headers = append(headers, &corev3.HeaderValue{Key: k, Value: v})
	}
	return &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{Headers: headers}}
}

func testPathResolver() *models.PathResolver {
	r, err := models.NewPathResolver(map[string]string{
		"/v1/chat/completions": "chat_completions",
		"/v1/messages":         "messages",
		"/v1/responses":        "responses",
	})
	if err != nil {
		panic(err)
	}
	return r
}

func newTestProcessor(global models.GuardrailsSettings, masker *fakeMasker, st *fakeStateStore) requestProcessor {
	return newRequestProcessor(testConfig(), masker, &fakeSettings{global: global}, nil, st, nil, testPathResolver())
}

func maskedState() models.MaskingState {
	return models.MaskingState{
		TriggeredRuleIDs:   []string{"email"},
		TriggeredDataTypes: []models.DataType{models.DataTypePERSONALDATA},
		Replacements: []models.Replacement{
			{RuleID: "email", Original: "user@example.com", Placeholder: "<EMAIL_1>"},
		},
	}
}

// maskedStateFmt is maskedState with the resolved wire format recorded, as the
// enforce path persists and holds it after masking.
func maskedStateFmt(f models.APIFormat) models.MaskingState {
	s := maskedState()
	s.Format = f
	return s
}

const chatBody = `{"model":"gpt-x","messages":[{"role":"user","content":"my mail is user@example.com"}]}`

// --- tests ---

func TestHandleRequestHeadersDisabledSkipsAll(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(models.GuardrailsSettings{Enabled: false}, &fakeMasker{}, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)
	assert.True(t, proc.ShouldSkip(StepAll))
}

func TestHandleRequestHeadersOverrideNarrows(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(allEnabled(), masker, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:                "/v1/chat/completions",
		"x-guardrails-data-types": "personal_data",
	}))
	require.NoError(t, err)
	require.False(t, proc.ShouldSkip(StepAll))
	assert.Equal(t, []models.DataType{models.DataTypePERSONALDATA}, proc.Settings.DataTypes)

	// The narrowed set is what reaches the masker.
	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
	assert.Equal(t, []models.DataType{models.DataTypePERSONALDATA}, masker.gotDataTypes)
}

func TestHandleRequestHeadersOverrideNoneSkips(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:                "/v1/chat/completions",
		"x-guardrails-data-types": "none",
	}))
	require.NoError(t, err)
	assert.True(t, proc.ShouldSkip(StepAll))
}

func TestHandleRequestHeadersGarbageOverrideKeepsFullProtection(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:                "/v1/chat/completions",
		"x-guardrails-data-types": "not-a-type,42",
	}))
	require.NoError(t, err)
	require.False(t, proc.ShouldSkip(StepAll))
	assert.Equal(t, allEnabled().DataTypes, proc.Settings.DataTypes)
}

func TestHandleRequestBodyPersistsMaskingState(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/chat/completions",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)

	// The store key is derived from x-request-id, not the raw value.
	require.NotEqual(t, "req-123", proc.Md.StateKey)
	stored, err := st.GetMaskingState(context.Background(), proc.Md.StateKey)
	require.NoError(t, err)
	assert.Equal(t, maskedStateFmt(models.APIFormatChatCompletions), stored)
}

// The masking-state persist must survive cancellation of the ext_proc stream
// context (client disconnect): its write uses context.WithoutCancel so a
// response-only replica still finds the state and does not leak placeholders.
func TestHandleRequestBodyPersistDetachedFromStreamCancel(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/chat/completions",
		requestIDHeader: "req-cancel",
	}))
	require.NoError(t, err)

	// Cancel the stream context before the body phase runs, mimicking an Envoy
	// stream cancellation / client disconnect mid-request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = proc.HandleRequestBody(ctx, &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)

	// The persist still happened, and with a live (non-cancelled) context — the
	// detach dropped cancellation propagation while keeping the timeout budget.
	require.Equal(t, 1, st.puts, "persist must run despite stream cancellation")
	require.NoError(t, st.putCtxErr, "persist context must not be cancelled")
	stored, getErr := st.GetMaskingState(context.Background(), proc.Md.StateKey)
	require.NoError(t, getErr)
	assert.Equal(t, maskedStateFmt(models.APIFormatChatCompletions), stored)
}

// A /v1/messages request must mask the top-level system prompt: the body sent
// upstream carries the placeholder, not the original secret.
func TestHandleRequestBodyMessagesMasksSystem(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	// Fields are extracted system-first, then message content; the masker
	// replaces the system value with a placeholder and leaves the rest.
	masker := &fakeMasker{resp: mask.CommandResponse{
		MaskingState: maskedState(),
		MaskedTexts:  []string{"<CARD_1>", "hi"},
	}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/messages",
		requestIDHeader: "req-sys",
	}))
	require.NoError(t, err)

	body := []byte(`{"model":"claude-x","system":"card 4111 1111 1111 1111",` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: body})
	require.NoError(t, err)

	out := resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
	assert.Equal(t, "<CARD_1>", gjson.GetBytes(out, "system").String(), "system must be masked")
	assert.NotContains(t, string(out), "4111 1111 1111 1111", "original secret must not reach upstream")
}

func TestHandleRequestBodyStorePutErrorStillMasks(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	st.putErr = errors.New("store down")
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)

	resp, err := proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
	require.NotNil(t, resp.GetRequestBody())
	// Masking state stays usable in-process despite the store failure.
	assert.Equal(t, maskedStateFmt(models.APIFormatChatCompletions), proc.MaskingState)
}

func TestHandleRequestBodyRecordsAudit(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	rec := &fakeAuditRecorder{}
	proc := newTestProcessor(allEnabled(), masker, newFakeStateStore())
	proc.Audit = rec

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/chat/completions",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)

	require.Len(t, rec.calls, 1)
	assert.Equal(t, "req-123", rec.calls[0].md.RequestID)
	assert.Equal(t, maskedStateFmt(models.APIFormatChatCompletions), rec.calls[0].st)
	assert.NotEmpty(t, rec.calls[0].maskedTexts)
}

func TestHandleRequestHeadersPrefixedPathResolvesFormat(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/openai/v1/messages",
	}))
	require.NoError(t, err)
	assert.False(t, proc.ShouldSkip(StepAll), "proxy-prefixed path must be accepted via suffix match")
	assert.Equal(t, models.APIFormatMessages, proc.Md.Format)
}

func TestHandleRequestBodyResponsesFormatMasksInput(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	masker := &fakeMasker{resp: mask.CommandResponse{
		MaskingState: maskedState(),
		MaskedTexts:  []string{"my mail is <EMAIL_1>"},
	}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/responses",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)
	assert.Equal(t, models.APIFormatResponses, proc.Md.Format)

	body := `{"model":"gpt-x","input":"my mail is user@example.com"}`
	resp, err := proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(body)})
	require.NoError(t, err)

	mutated := resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
	require.NotNil(t, mutated)
	assert.Contains(t, string(mutated), `"input":"my mail is <EMAIL_1>"`)
}

// Anthropic /v1/messages requests must mask text, tool_use.input (a JSON
// object, masked per string leaf so it stays an object) and tool_result.content.
func TestHandleRequestBodyMessagesFormatMasksToolBlocks(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	// MaskedTexts align with the fields extracted in document order:
	// content.0.text, content.1.input.q (string leaf), content.2.content.
	masker := &fakeMasker{resp: mask.CommandResponse{
		MaskingState: maskedState(),
		MaskedTexts: []string{
			"my mail is <EMAIL_1>",
			"<EMAIL_1>",
			"see <EMAIL_1>",
		},
	}}
	proc := newTestProcessor(allEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/messages",
		requestIDHeader: "req-msg-1",
	}))
	require.NoError(t, err)
	assert.Equal(t, models.APIFormatMessages, proc.Md.Format)

	body := `{"model":"claude","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"my mail is user@example.com"},` +
		`{"type":"tool_use","id":"t1","name":"f","input":{"q":"user@example.com"}},` +
		`{"type":"tool_result","tool_use_id":"t1","content":"see user@example.com"}` +
		`]}]}`
	resp, err := proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(body)})
	require.NoError(t, err)

	mutated := resp.GetRequestBody().GetResponse().GetBodyMutation().GetBody()
	require.NotNil(t, mutated)
	got := string(mutated)
	assert.Contains(t, got, `"text":"my mail is <EMAIL_1>"`)
	assert.Contains(t, got, `"content":"see <EMAIL_1>"`)
	// tool_use.input stays a JSON object (raw), not a stringified value.
	assert.Contains(t, got, `"input":{"q":"<EMAIL_1>"}`)
	assert.NotContains(t, got, `"input":"{`)
}

func detectEnabled() models.GuardrailsSettings {
	gs := allEnabled()
	gs.Mode = models.ModeDetect
	return gs
}

func TestHandleRequestBodyDetectModePassesThrough(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(detectEnabled(), masker, st)

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/chat/completions",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)
	assert.Equal(t, models.ModeDetect, proc.Md.Mode)

	resp, err := proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)

	// No body mutation: the request continues unchanged (mode-override skip,
	// not a BodyResponse with a mutation).
	assert.Nil(t, resp.GetRequestBody().GetResponse().GetBodyMutation())
	assert.True(t, proc.ShouldSkip(StepAll))

	// No in-process or persisted state -> the response phase cannot demask.
	assert.True(t, proc.MaskingState.IsEmpty())
	assert.Zero(t, st.puts)

	// Stream end must not issue a store delete (nothing was persisted).
	require.NoError(t, proc.Close())
	assert.Zero(t, st.deletes)
}

func TestHandleRequestBodyDetectModeRecordsAudit(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	rec := &fakeAuditRecorder{}
	proc := newTestProcessor(detectEnabled(), masker, newFakeStateStore())
	proc.Audit = rec

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader:      "/v1/chat/completions",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)

	require.Len(t, rec.calls, 1)
	assert.Equal(t, "req-123", rec.calls[0].md.RequestID)
	assert.Equal(t, models.ModeDetect, rec.calls[0].md.Mode)
	assert.Equal(t, maskedState(), rec.calls[0].st)
	assert.NotEmpty(t, rec.calls[0].maskedTexts)
}

func TestHandleRequestBodyDetectModeNoReplacementsSkipsAudit(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{} // triggers nothing
	rec := &fakeAuditRecorder{}
	proc := newTestProcessor(detectEnabled(), masker, newFakeStateStore())
	proc.Audit = rec

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
	assert.Empty(t, rec.calls)
}

func TestHandleRequestBodyNoReplacementsSkipsAudit(t *testing.T) {
	t.Parallel()
	// Masker triggers nothing: no replacements -> no audit record.
	masker := &fakeMasker{}
	rec := &fakeAuditRecorder{}
	proc := newTestProcessor(allEnabled(), masker, newFakeStateStore())
	proc.Audit = rec

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
	assert.Empty(t, rec.calls)
}

func TestHandleRequestBodyNilAuditRecorderIsSafe(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{resp: mask.CommandResponse{MaskingState: maskedState()}}
	proc := newTestProcessor(allEnabled(), masker, newFakeStateStore()) // Audit == nil

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
}

func TestHandleRequestBodyMaskerErrorFailsOpen(t *testing.T) {
	t.Parallel()
	masker := &fakeMasker{err: errors.New("boom")}
	proc := newTestProcessor(allEnabled(), masker, newFakeStateStore())

	_, err := proc.HandleRequestHeaders(context.Background(), requestHeaders(map[string]string{
		pathHeader: "/v1/chat/completions",
	}))
	require.NoError(t, err)

	_, err = proc.HandleRequestBody(context.Background(), &extprocv3.HttpBody{Body: []byte(chatBody)})
	require.NoError(t, err)
	assert.True(t, proc.ShouldSkip(StepAll))
}

func TestHandleResponseHeadersFallsBackToStore(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	// Simulate the response phase on a replica that did not see the request
	// body: in-process MaskingState is empty and Metadata is unset — only the
	// store has the state, keyed by the derived store key. The response phase
	// re-derives that key from the response's x-request-id.
	seeded := maskedStateFmt(models.APIFormatMessages)
	storeKey := deriveStateKey("", "req-123")
	st.entries[storeKey] = seeded

	proc := newTestProcessor(allEnabled(), &fakeMasker{}, st)

	resp, err := proc.HandleResponseHeaders(context.Background(), requestHeaders(map[string]string{
		"content-type":  "application/json",
		requestIDHeader: "req-123",
	}))
	require.NoError(t, err)
	require.False(t, proc.ShouldSkip(StepAll))
	assert.Equal(t, seeded, proc.MaskingState)
	// The wire format resolved by the request phase is recovered from the store.
	assert.Equal(t, models.APIFormatMessages, proc.Md.Format)

	// The triggered data types header is emitted from the recovered state.
	mutation := resp.GetResponseHeaders().GetResponse().GetHeaderMutation()
	require.NotNil(t, mutation)
}

func TestHandleResponseHeadersNoStateSkips(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())
	proc.Md = models.Metadata{RequestID: "req-unknown", Path: "/v1/chat/completions"}

	_, err := proc.HandleResponseHeaders(context.Background(), requestHeaders(map[string]string{
		"content-type": "application/json",
	}))
	require.NoError(t, err)
	assert.True(t, proc.ShouldSkip(StepAll))
}

func TestCloseDeletesStoredState(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	storeKey := deriveStateKey("", "req-123")
	st.entries[storeKey] = maskedState()

	proc := newTestProcessor(allEnabled(), &fakeMasker{}, st)
	proc.Md = models.Metadata{RequestID: "req-123", StateKey: storeKey}
	proc.MaskingState = maskedState()

	require.NoError(t, proc.Close())
	assert.Equal(t, 1, st.deletes)
	assert.Empty(t, st.entries)
}

func TestHandleResponseBodySseErrorFailsOpen(t *testing.T) {
	t.Parallel()
	proc := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())
	proc.IsSse = true
	proc.sseProcessor = &fakeSseProcessor{err: errors.New("boom")}

	chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	resp, err := proc.handleResponseBodySse(context.Background(), &extprocv3.HttpBody{Body: chunk, EndOfStream: true})

	// Fail-open: an SSE demask error never breaks the stream — no error is
	// returned and the offending chunk is forwarded unchanged.
	require.NoError(t, err)
	require.NotNil(t, resp)
	got := resp.GetResponseBody().GetResponse().GetBodyMutation().GetStreamedResponse().GetBody()
	assert.Equal(t, chunk, got)
}

func TestCloseSkipsDeleteWhenDisabled(t *testing.T) {
	t.Parallel()
	st := newFakeStateStore()
	storeKey := deriveStateKey("", "req-123")
	st.entries[storeKey] = maskedState()

	proc := newTestProcessor(allEnabled(), &fakeMasker{}, st)
	proc.Conf.Guardrails.StateDeleteOnClose = false
	proc.Md = models.Metadata{RequestID: "req-123", StateKey: storeKey}
	proc.MaskingState = maskedState()

	require.NoError(t, proc.Close())
	// With delete-on-close disabled the entry survives for the response phase
	// on another replica; the MaskingTTL reclaims it instead.
	assert.Zero(t, st.deletes)
	assert.Len(t, st.entries, 1)
}
