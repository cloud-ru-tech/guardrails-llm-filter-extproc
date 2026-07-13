package extproc

import (
	"context"
	"encoding/json"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/guardrails/demask"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/guardrails/regex/scanners/placeholder"
)

// fakeDemaskReg / fakeDemaskScanner let a real demask.Factory run in-process
// using only its exact-replacer path (placeholder -> original), which is all
// these tests need.
type fakeDemaskReg struct{}

func (fakeDemaskReg) GetMaxPlaceholderLenByRuleIDs(...string) int { return 32 }

type fakeDemaskScanner struct{}

func (fakeDemaskScanner) Scan(string, []string) ([]placeholder.Match, error) { return nil, nil }

// procWithDemask wires a processor with a real demasker for the given
// replacements (TriggeredRuleIDs left empty so only the exact replacer runs).
func procWithDemask(reps []models.Replacement) *requestProcessor {
	p := newTestProcessor(allEnabled(), &fakeMasker{}, newFakeStateStore())
	state := models.MaskingState{Replacements: reps}
	p.MaskingState = state
	p.DemaskerProvider = demask.NewProvider(fakeDemaskReg{}, fakeDemaskScanner{})
	p.DemaskerFactory = p.DemaskerProvider.NewFactory(state)
	return &p
}

// A tool-call argument whose original value contains a double quote must be
// demasked and re-escaped so the arguments stay valid JSON — the placeholder
// must not leak to the client just because the original had a JSON metachar.
func TestHandleOpenAICompletion_ToolArgsOriginalWithQuote(t *testing.T) {
	t.Parallel()
	proc := procWithDemask([]models.Replacement{
		{RuleID: "secret", Original: `ab"cd`, Placeholder: "<SECRET_1>"},
	})

	body := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"key\":\"<SECRET_1>\"}"}}]}}]}`)

	out := proc.handleOpenAICompletion(context.Background(), body)

	// Output must be valid JSON overall.
	require.True(t, json.Valid(out), "response body must be valid JSON: %s", out)

	args := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String()
	require.True(t, json.Valid([]byte(args)), "tool-call arguments must be valid JSON: %s", args)

	// The placeholder must be gone and the real value restored.
	assert.NotContains(t, args, "<SECRET_1>", "placeholder leaked to the client")
	assert.Equal(t, `ab"cd`, gjson.Get(args, "key").String())
}

// A tool_use block with an absent/empty Input must not break demasking of the
// rest of the message (e.g. a sibling text block).
func TestHandleAnthropicMessage_EmptyToolInputKeepsTextDemasked(t *testing.T) {
	t.Parallel()
	proc := procWithDemask([]models.Replacement{
		{RuleID: "email", Original: "alice@example.com", Placeholder: "<EMAIL_1>"},
	})

	body := []byte(`{"role":"assistant","content":[` +
		`{"type":"text","text":"reach me at <EMAIL_1>"},` +
		`{"type":"tool_use","id":"t1","name":"f"}` +
		`]}`)

	out := proc.handleAnthropicMessage(context.Background(), body)
	require.True(t, json.Valid(out), "message must be valid JSON: %s", out)

	// The sibling text block must be demasked, not left masked because the
	// tool_use block had no input.
	txt := gjson.GetBytes(out, "content.0.text").String()
	assert.Equal(t, "reach me at alice@example.com", txt)
}

// Demasking a chat/completion must not drop unmodeled fields or inject a usage
// object. The extract+patch path leaves everything it does not demask byte-for
// -byte.
func TestHandleOpenAICompletion_PreservesUnknownFields(t *testing.T) {
	t.Parallel()
	proc := procWithDemask([]models.Replacement{
		{RuleID: "email", Original: "a@b.com", Placeholder: "<EMAIL_1>"},
	})

	body := []byte(`{"id":"x","object":"chat.completion","model":"m","system_fingerprint":"fp_1",` +
		`"service_tier":"scale","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"email <EMAIL_1>","refusal":null},"logprobs":{"content":[]}}]}`)

	out := proc.handleOpenAICompletion(context.Background(), body)
	require.True(t, json.Valid(out), "response body must be valid JSON: %s", out)

	assert.Equal(t, "fp_1", gjson.GetBytes(out, "system_fingerprint").String())
	assert.Equal(t, "scale", gjson.GetBytes(out, "service_tier").String())
	assert.True(t, gjson.GetBytes(out, "choices.0.logprobs").Exists(), "logprobs must be preserved")
	assert.False(t, gjson.GetBytes(out, "usage").Exists(), "usage must not be injected")
	assert.Equal(t, "email a@b.com", gjson.GetBytes(out, "choices.0.message.content").String())
}

// An unknown/empty response format must pass the body through unchanged rather
// than rewrite it (fail-open). Regression for the switch default overwriting a
// non-chat body with an empty chat skeleton.
func TestHandleResponseBodyFull_UnknownFormatPassthrough(t *testing.T) {
	t.Parallel()
	proc := procWithDemask(nil)
	proc.Md.Format = "" // simulate pre-Format masking state (rolling upgrade)

	in := []byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`)
	body := &extprocv3.HttpBody{Body: in, EndOfStream: true}

	resp, err := proc.handleResponseBodyFull(context.Background(), body)
	require.NoError(t, err)
	out := resp.GetResponseBody().GetResponse().GetBodyMutation().GetStreamedResponse().GetBody()
	require.JSONEq(t, string(in), string(out))
}

// The plain (no-metachar) case must keep working unchanged.
func TestHandleOpenAICompletion_ToolArgsPlainOriginal(t *testing.T) {
	t.Parallel()
	proc := procWithDemask([]models.Replacement{
		{RuleID: "email", Original: "user@example.com", Placeholder: "<EMAIL_1>"},
	})

	body := []byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"c","type":"function","function":{"name":"f","arguments":"{\"to\":\"<EMAIL_1>\"}"}}]}}]}`)

	out := proc.handleOpenAICompletion(context.Background(), body)
	require.True(t, json.Valid(out))
	args := gjson.GetBytes(out, "choices.0.message.tool_calls.0.function.arguments").String()
	assert.Equal(t, "user@example.com", gjson.Get(args, "to").String())
}

// Demasking a masked /v1/messages response must not fabricate fields the
// response never had (stop_details, container, union zero values), must not
// coerce null to "", and must preserve fields unknown to any SDK version.
// Regression for the typed anthropic.Message round-trip.
func TestHandleAnthropicMessage_PreservesBodyShape(t *testing.T) {
	t.Parallel()
	proc := procWithDemask([]models.Replacement{
		{RuleID: "email", Original: "alice@example.com", Placeholder: "<EMAIL_1>"},
		{RuleID: "secret", Original: `say "hi"`, Placeholder: "<SECRET_1>"},
	})

	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
		`"future_field":{"a":1},` +
		`"content":[` +
		`{"type":"text","text":"hello <EMAIL_1>"},` +
		`{"type":"thinking","thinking":"about <EMAIL_1>","signature":"enc"},` +
		`{"type":"tool_use","id":"t1","name":"f","input":{"note":"<SECRET_1>"}},` +
		`{"type":"redacted_thinking","data":"opaque"}` +
		`],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2}}`)

	out := proc.handleAnthropicMessage(context.Background(), body)
	require.True(t, json.Valid(out), "message must be valid JSON: %s", out)

	// Demasked fields.
	assert.Equal(t, "hello alice@example.com", gjson.GetBytes(out, "content.0.text").String())
	assert.Equal(t, "about alice@example.com", gjson.GetBytes(out, "content.1.thinking").String())
	input := gjson.GetBytes(out, "content.2.input")
	require.True(t, input.IsObject(), "tool_use input must stay an object: %s", input.Raw)
	assert.Equal(t, `say "hi"`, input.Get("note").String())

	// Nothing fabricated, nothing dropped, null stays null.
	assert.False(t, gjson.GetBytes(out, "stop_details").Exists(), "stop_details must not be fabricated")
	assert.False(t, gjson.GetBytes(out, "container").Exists(), "container must not be fabricated")
	assert.Equal(t, gjson.Null, gjson.GetBytes(out, "stop_sequence").Type, "null must stay null")
	assert.Equal(t, "enc", gjson.GetBytes(out, "content.1.signature").String())
	assert.Equal(t, "opaque", gjson.GetBytes(out, "content.3.data").String())
	assert.Equal(t, int64(1), gjson.GetBytes(out, "future_field.a").Int(), "unknown fields must survive")
	assert.False(t, gjson.GetBytes(out, "content.0.citations").Exists(), "no union zero values injected")
}
