// Package config holds the env-driven service configuration.
// All variables share the GUARDRAILS_ prefix (e.g. GUARDRAILS_GRPC_ADDR).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
)

// EnvPrefix is prepended to every environment variable name.
const EnvPrefix = "GUARDRAILS_"

// DefaultGuardrailPaths are the built-in request paths always guarded.
// GUARDRAILS_PATHS entries are merged on top of these (a user entry for the
// same path wins); core paths cannot be silently dropped by a partial
// override. See Guardrails.Paths.
var DefaultGuardrailPaths = map[string]string{
	"/v1/chat/completions": "chat_completions",
	"/v1/messages":         "messages",
	"/v1/responses":        "responses",
}

type Config struct {
	// Logging
	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`  // debug|info|warn|error
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"` // json|text

	// Servers
	GRPCAddr    string `env:"GRPC_ADDR" envDefault:":9000"`
	HealthPort  int    `env:"HEALTH_PORT" envDefault:"9005"`
	MetricsPort int    `env:"METRICS_PORT" envDefault:"9091"`

	// GrpcSecure enables TLS with a self-signed certificate on the ext_proc
	// gRPC listener. Plaintext is the default because Envoy usually talks to
	// the processor over loopback or an mTLS service mesh.
	//
	// SECURITY: this channel carries the ORIGINAL unmasked request body (the
	// very PII/secrets this service strips) from Envoy, and the demasked
	// response body with real secrets restored, back to Envoy. Plaintext
	// (false) is only safe inside an mTLS mesh or over loopback; anywhere the
	// hop can be observed you MUST set GRPC_SECURE=true.
	GrpcSecure bool `env:"GRPC_SECURE" envDefault:"false"`

	GuardrailsRules   GuardrailsRulesCfg `envPrefix:"RULES_"`
	GuardrailsHeaders GuardrailsHeaders  `envPrefix:"HEADERS_"`

	Guardrails Guardrails `envPrefix:""`
	Store      Store      `envPrefix:"STORE_"`
	API        API        `envPrefix:"API_"`
	Audit      Audit      `envPrefix:"AUDIT_"`
}

// Audit StoreOriginalTexts modes.
const (
	OriginalsOff       = "off"
	OriginalsPlain     = "plain"
	OriginalsEncrypted = "encrypted"
)

// Audit configures the per-request masking audit trail. Records describe
// what was masked (rules, data types, placeholders); original sensitive
// values are stored only when StoreOriginalTexts opts in (default off).
type Audit struct {
	// Enabled turns on audit-record writing and the /v1/audit API endpoints.
	Enabled bool `env:"ENABLED" envDefault:"false"`

	// StoreMaskedTexts additionally persists the masked (placeholder-
	// substituted) request texts in each record. Still no originals, but
	// prompts are user content: enable only with an access-controlled store
	// and a non-empty API token.
	StoreMaskedTexts bool `env:"STORE_MASKED_TEXTS" envDefault:"false"`

	// StoreMaskedResponseTexts additionally persists the masked (placeholder-
	// substituted) model response texts in each record — the response
	// counterpart of StoreMaskedTexts. Off by default; same sensitivity class.
	StoreMaskedResponseTexts bool `env:"STORE_MASKED_RESPONSE_TEXTS" envDefault:"false"`

	// StoreOriginalTexts controls whether each audit replacement carries the
	// raw pre-masking value behind its placeholder (for the UI "reveal on
	// hover" feature). One of:
	//   - "off"       (default) — never store originals; the invariant holds.
	//   - "plain"     — store originals unencrypted in the record.
	//   - "encrypted" — store originals encrypted with the store AES-256-GCM
	//                   key (GUARDRAILS_STORE_ENCRYPTION_*); requires encryption
	//                   to be enabled or startup fails.
	// SECURITY: "plain" and "encrypted" persist raw sensitive values; use only
	// with an access-controlled store.
	StoreOriginalTexts string `env:"STORE_ORIGINAL_TEXTS" envDefault:"off"`

	// Retention is how long audit records are kept in the repository.
	Retention time.Duration `env:"RETENTION" envDefault:"24h"`

	// MaxEntries caps the audit map of the in_memory backend (oldest record
	// evicted first); ignored by redis/postgres. 0 = unlimited.
	MaxEntries int `env:"MAX_ENTRIES" envDefault:"10000"`
}

// Guardrails is the global policy applied to all traffic. It seeds the
// settings store on first start; afterwards the store (mutable via the
// configuration API) is the source of truth.
type Guardrails struct {
	Enabled bool `env:"ENABLED" envDefault:"true"`

	// Mode selects enforcement: "enforce" masks traffic, "detect" (shadow
	// mode) only scans and records metrics/audit without mutating bodies.
	Mode string `env:"MODE" envDefault:"enforce"`

	// DataTypes is a comma-separated list of enabled data types, by number
	// or name (e.g. "1,2,3" or "credentials,personal_data"). 6 (CUSTOM) is
	// included so custom rules created via the API — which the docs and
	// /v1/data-types steer toward data_type=CUSTOM — actually scan by default;
	// omitting it silently disables every CUSTOM rule regardless of its own
	// enabled flag. No built-in rule uses CUSTOM, so it is inert until a custom
	// rule exists.
	DataTypes string `env:"DATA_TYPES" envDefault:"1,2,3,4,5,6"`

	// KeywordPrefilterEnabled turns on the keyword pre-filter: a rule that
	// declares keywords runs its regex only when at least one keyword is
	// present in the text (case-insensitive). Off by default — enabling it
	// trades detection recall for scan speed and only helps rules whose
	// keyword lists are accurate.
	KeywordPrefilterEnabled bool `env:"KEYWORD_PREFILTER_ENABLED" envDefault:"false"`

	// MaskParallelMinBytes is the combined request-text size (in bytes) at or
	// above which the masking scan fans out across text fields; smaller bodies
	// are scanned sequentially, avoiding goroutine overhead on the latency-
	// sensitive small-request hot path. A request must also carry at least two
	// text fields to parallelize. 0 falls back to the built-in default.
	MaskParallelMinBytes int `env:"MASK_PARALLEL_MIN_BYTES" envDefault:"8192"`

	// Paths maps request paths to API wire formats as comma-separated
	// path:format pairs. Matching is exact first, then longest suffix, so
	// proxy-prefixed mounts (/openai/v1/chat/completions) work without
	// configuration. Entries are MERGED on top of DefaultGuardrailPaths (a
	// user entry for the same path wins), so a partial GUARDRAILS_PATHS can
	// never silently disable masking for a core endpoint. Validated at boot.
	Paths map[string]string `env:"PATHS"`

	// OverrideHeader names the trusted request header that narrows the
	// enabled data types per request. Empty disables the override. Envoy
	// MUST strip this header from client requests.
	OverrideHeader string `env:"OVERRIDE_HEADER" envDefault:"x-guardrails-data-types"`

	// Refresh intervals converge replicas on API changes when the store
	// backend is shared (redis/postgres). 0 disables refreshing.
	SettingsRefreshInterval time.Duration `env:"SETTINGS_REFRESH_INTERVAL" envDefault:"30s"`
	RulesRefreshInterval    time.Duration `env:"RULES_REFRESH_INTERVAL" envDefault:"30s"`

	// StateKeySalt salts the masking-state store key, which is derived as
	// HMAC-SHA256(salt, x-request-id). This keeps a client-supplied or
	// predictable x-request-id from being used to guess or collide with
	// another request's key (the state holds original sensitive values).
	// It MUST be identical across replicas for the cross-replica store
	// fallback to resolve the same key. Empty falls back to an unkeyed
	// SHA-256 of the x-request-id (still never uses the raw external value as
	// the key) and logs a startup warning. SECURITY: never log this value.
	StateKeySalt string `env:"STATE_KEY_SALT"`

	// StateDeleteOnClose deletes the persisted masking state when the ext_proc
	// stream closes. Keep true for single-replica / same-stream deployments.
	// Set false when request and response phases may be served by different
	// replicas off a shared store, so a request-side close cannot delete the
	// state the response side still needs — the MaskingTTL reclaims it instead.
	StateDeleteOnClose bool `env:"STATE_DELETE_ON_CLOSE" envDefault:"true"`
}

// API configures the management API: a gRPC service (GuardrailsApi) fronted
// by a grpc-gateway REST proxy.
type API struct {
	// GRPCAddr is the management gRPC listen address (GuardrailsApi), separate
	// from the ext_proc gRPC listener on GRPCAddr/:9000.
	GRPCAddr string `env:"GRPC_ADDR" envDefault:":9090"`

	// Addr is the REST gateway listen address; empty disables the whole
	// management API server (both REST and gRPC).
	//
	// The API is unauthenticated: it exposes mutating endpoints (rules,
	// settings) with no token check, so it must be protected at the network
	// layer (cluster-internal only, never public ingress).
	Addr string `env:"ADDR" envDefault:":9080"`
}

// Store selects and configures the persistence backend for masking state,
// custom rules and global settings.
type Store struct {
	// Backend is one of: in_memory (default), redis, postgres.
	Backend string `env:"BACKEND" envDefault:"in_memory"`

	// MaskingTTL is the safety-net TTL for masking-state entries; it must
	// exceed the longest expected streaming response.
	MaskingTTL time.Duration `env:"MASKING_TTL" envDefault:"15m"`

	Redis       StoreRedis `envPrefix:"REDIS_"`
	PostgresDSN string     `env:"POSTGRES_DSN"`

	// EncryptionEnabled turns on AES-256-GCM encryption of masking state at
	// rest for the external backends (redis, postgres); no-op for in_memory.
	EncryptionEnabled bool `env:"ENCRYPTION_ENABLED" envDefault:"false"`

	// EncryptionKey is the standard base64 encoding of a 32-byte key
	// (`openssl rand -base64 32`). Required when EncryptionEnabled; a missing
	// or malformed key fails startup. SECURITY: never log this value.
	EncryptionKey string `env:"ENCRYPTION_KEY"`
}

// StoreRedis holds connection parameters for the redis store backend.
type StoreRedis struct {
	Addr     string `env:"ADDR" envDefault:"redis:6379"`
	Password string `env:"PASSWORD"`
	DB       int    `env:"DB" envDefault:"0"`
}

type GuardrailsHeaders struct {
	DataTypesHeader      string `env:"DATA_TYPES_HEADER" envDefault:"x-guardrails-data-types-triggered"`
	TriggeredRulesHeader string `env:"TRIGGERED_RULES_HEADER" envDefault:"x-guardrails-triggered-rules"`

	// ExposeTriggeredRules controls whether the triggered-rules header is
	// added to responses. Rule IDs reveal which detectors fired, so exposing
	// them to end clients is opt-in.
	ExposeTriggeredRules bool `env:"EXPOSE_TRIGGERED_RULES" envDefault:"false"`
}

type GuardrailsRulesCfg struct {
	RegexRulesFile         string `env:"REGEX_RULES_FILE" envDefault:"./configs/guardrails_regex_rules.yaml"`
	GitleaksRegexRulesFile string `env:"GITLEAKS_REGEX_RULES_FILE" envDefault:"./configs/guardrails_regex_rules.gitleaks.generated.yaml"`

	// MaxCustom bounds the number of custom rules that can be created via the
	// configuration API. Each rule is evaluated against every request on the
	// hot path, so an unbounded count degrades latency/memory. 0 disables the
	// limit.
	MaxCustom int `env:"MAX_CUSTOM" envDefault:"500"`
	// MaxPatternLen bounds a custom rule's regex length. RE2 has no
	// backtracking, but a very long pattern still costs linearly per request.
	// 0 disables the limit.
	MaxPatternLen int `env:"MAX_PATTERN_LEN" envDefault:"4096"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.ParseWithOptions(cfg, env.Options{Prefix: EnvPrefix}); err != nil {
		return nil, err
	}
	// Merge the built-in core paths under any user overrides so a partial
	// GUARDRAILS_PATHS can never silently disable masking for a core endpoint.
	if cfg.Guardrails.Paths == nil {
		cfg.Guardrails.Paths = make(map[string]string, len(DefaultGuardrailPaths))
	}
	for path, format := range DefaultGuardrailPaths {
		if _, ok := cfg.Guardrails.Paths[path]; !ok {
			cfg.Guardrails.Paths[path] = format
		}
	}
	// Bad path config must fail the boot, not silently reject traffic.
	if _, err := models.NewPathResolver(cfg.Guardrails.Paths); err != nil {
		return nil, fmt.Errorf("parse %sPATHS: %w", EnvPrefix, err)
	}
	// A negative parallel-scan gate is meaningless and would be silently coerced
	// to the built-in default downstream, masking operator intent. 0 keeps the
	// built-in default; reject anything below it at boot.
	if cfg.Guardrails.MaskParallelMinBytes < 0 {
		return nil, fmt.Errorf("%sMASK_PARALLEL_MIN_BYTES must be >= 0, got %d", EnvPrefix, cfg.Guardrails.MaskParallelMinBytes)
	}
	// Header lookups go through extprocutils.HeadersToMap, which lower-cases
	// every header name. Normalize the configured override header name so a
	// mixed-case value still matches instead of silently never firing.
	cfg.Guardrails.OverrideHeader = strings.ToLower(strings.TrimSpace(cfg.Guardrails.OverrideHeader))
	// The audit originals mode is a small enum; reject unknown values at boot
	// rather than silently treating them as "off". "encrypted" additionally
	// requires store encryption to be configured — fail closed so an operator
	// cannot accidentally persist raw sensitive values in plaintext.
	switch cfg.Audit.StoreOriginalTexts {
	case OriginalsOff, OriginalsPlain:
	case OriginalsEncrypted:
		if !cfg.Store.EncryptionEnabled {
			return nil, fmt.Errorf("%sAUDIT_STORE_ORIGINAL_TEXTS=%q requires %sSTORE_ENCRYPTION_ENABLED=true with a valid %sSTORE_ENCRYPTION_KEY",
				EnvPrefix, OriginalsEncrypted, EnvPrefix, EnvPrefix)
		}
	default:
		return nil, fmt.Errorf("%sAUDIT_STORE_ORIGINAL_TEXTS must be one of %q, %q, %q; got %q",
			EnvPrefix, OriginalsOff, OriginalsPlain, OriginalsEncrypted, cfg.Audit.StoreOriginalTexts)
	}
	return cfg, nil
}
