package extproc

// redactedValue replaces a sensitive header value in debug logs.
const redactedValue = "[REDACTED]"

// sensitiveHeaders lists request/response header names whose values must never
// reach the logs. Through an LLM gateway these carry provider credentials and
// session material (see security policy 2.8). Keys are lower-case to match
// extprocutils.HeadersToMap, which lower-cases every header name.
var sensitiveHeaders = map[string]struct{}{
	authHeader:            {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"api-key":             {},
	// Provider-specific credential headers seen through LLM gateways.
	"x-goog-api-key":           {}, // Google / Gemini
	"x-goog-iam-token":         {},
	"x-amz-security-token":     {}, // AWS Bedrock (SigV4 session token)
	"x-aws-ec2-metadata-token": {},
	"openai-api-key":           {},
	"anthropic-api-key":        {},
}

// redactHeaders returns a shallow copy of headers with the values of
// sensitive headers replaced by a placeholder. The input map is never mutated.
func redactHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if _, ok := sensitiveHeaders[k]; ok {
			out[k] = redactedValue
			continue
		}
		out[k] = v
	}
	return out
}
