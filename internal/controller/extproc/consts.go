package extproc

import "time"

// stateStorePutTimeout bounds the masking-state persist on the request hot
// path. The write is best-effort (fail-open); this keeps a slow or hung store
// from blocking masking until the stream context is canceled.
const stateStorePutTimeout = 2 * time.Second

// Request headers extracted by the guardrails processor.
const (
	pathHeader      = ":path"
	requestIDHeader = "x-request-id"
	modelNameHeader = "x-gateway-model-name"

	authHeader = "authorization"

	// contentTypeHeader is inspected on responses to detect SSE.
	contentTypeHeader = "content-type"
)

// SSE content type sentinel.
const (
	ContentTypeSse = "text/event-stream"
)
