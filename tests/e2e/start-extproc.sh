#!/usr/bin/env bash
# Start the guardrails processor for local e2e testing. Runs from the repo root
# so it finds ./configs/*.yaml. Config is via GUARDRAILS_* env (overridable).
set -euo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HARNESS/../.." && pwd)"
cd "$REPO"

export GUARDRAILS_LOG_LEVEL="${GUARDRAILS_LOG_LEVEL:-debug}"
export GUARDRAILS_LOG_FORMAT="${GUARDRAILS_LOG_FORMAT:-text}"
export GUARDRAILS_STORE_BACKEND="${GUARDRAILS_STORE_BACKEND:-in_memory}"
export GUARDRAILS_AUDIT_ENABLED="${GUARDRAILS_AUDIT_ENABLED:-true}"
export GUARDRAILS_AUDIT_STORE_MASKED_TEXTS="${GUARDRAILS_AUDIT_STORE_MASKED_TEXTS:-true}"
export GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES="${GUARDRAILS_HEADERS_EXPOSE_TRIGGERED_RULES:-true}"
export GUARDRAILS_API_TOKEN="${GUARDRAILS_API_TOKEN:-e2e-secret-token}"
export GUARDRAILS_STATE_KEY_SALT="${GUARDRAILS_STATE_KEY_SALT:-e2e-salt-xyz}"

exec ./bin/extproc-guardrails
