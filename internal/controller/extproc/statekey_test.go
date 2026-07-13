package extproc

import "testing"

func TestDeriveStateKey(t *testing.T) {
	t.Parallel()

	// Empty request ID yields an empty key (caller then skips the store).
	if got := deriveStateKey("salt", ""); got != "" {
		t.Fatalf("empty requestID: got %q, want empty", got)
	}

	// Deterministic: same inputs → same key (request and response phases,
	// and replicas sharing a salt, must resolve the same key).
	a := deriveStateKey("salt", "req-1")
	b := deriveStateKey("salt", "req-1")
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}

	// The raw request ID is never used as the key.
	if a == "req-1" {
		t.Fatal("key equals raw request ID; must be hashed")
	}

	// Different request IDs → different keys.
	if a == deriveStateKey("salt", "req-2") {
		t.Fatal("distinct request IDs collided")
	}

	// The salt changes the key: a predictable request ID cannot be used to
	// reconstruct the key without the salt.
	if a == deriveStateKey("other-salt", "req-1") {
		t.Fatal("salt had no effect on the derived key")
	}

	// Empty salt still hashes (unkeyed SHA-256) rather than passing through.
	unsalted := deriveStateKey("", "req-1")
	if unsalted == "" || unsalted == "req-1" {
		t.Fatalf("empty-salt derivation invalid: %q", unsalted)
	}
	if unsalted == a {
		t.Fatal("salted and unsalted keys must differ")
	}
}
