package extproc

import (
	"context"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
)

// An unresolvable path must pass through unchanged (mask/pass — never block),
// not return an ImmediateResponse that rejects the request.
func TestHandleRequestHeadersUnsupportedPathPassesThrough(t *testing.T) {
	t.Parallel()

	proc := requestProcessor{PathResolver: testPathResolver()}
	resp, err := proc.HandleRequestHeaders(context.Background(), &extprocv3.HttpHeaders{
		Headers: &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{
				{Key: pathHeader, Value: "/v1/models"},
			},
		},
	})

	require.NoError(t, err)
	require.True(t, proc.ShouldSkip(StepAll))

	// No ImmediateResponse (no block); the request continues unchanged.
	require.Nil(t, resp.GetImmediateResponse(), "unsupported path must not be rejected")
}
