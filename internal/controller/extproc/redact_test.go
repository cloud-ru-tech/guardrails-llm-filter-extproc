package extproc

import "testing"

func TestRedactHeaders(t *testing.T) {
	in := map[string]string{
		"authorization":       "Bearer sk-secret",
		"proxy-authorization": "Basic abc",
		"cookie":              "session=abc",
		"set-cookie":          "session=abc",
		"x-api-key":           "key-123",
		"api-key":             "key-456",
		":path":               "/v1/chat/completions",
		"content-type":        "application/json",
		"x-request-id":        "req-1",
	}

	out := redactHeaders(in)

	for _, k := range []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key"} {
		if out[k] != redactedValue {
			t.Errorf("header %q = %q, want %q", k, out[k], redactedValue)
		}
	}
	for _, k := range []string{":path", "content-type", "x-request-id"} {
		if out[k] != in[k] {
			t.Errorf("non-sensitive header %q = %q, want %q", k, out[k], in[k])
		}
	}

	// The input map must not be mutated.
	if in["authorization"] != "Bearer sk-secret" {
		t.Errorf("input map was mutated: authorization = %q", in["authorization"])
	}
}
