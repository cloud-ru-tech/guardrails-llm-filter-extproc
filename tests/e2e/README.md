# Local e2e harness

A self-contained end-to-end test harness for `extproc-guardrails`. It drives the
**real Envoy ext_proc data path** and verifies masking (request side) and
demasking (response side) across all three APIs — `/v1/chat/completions`,
`/v1/messages`, `/v1/responses` — streaming and non-streaming, including
tool-call arguments and SSE placeholders split across frames.

> This harness lives under `tests/e2e/`. The Python drivers and mock upstream are
> committed; generated artifacts (logs, `capture/`, `results/`, built binaries,
> plan files) are git-ignored.

## Topology

```
 curl / python driver
        │  headers: X-Test-Id, X-Chunk-Runes, X-Upstream-Mode, x-guardrails-data-types
        ▼
 Envoy (docker :10000, FULL_DUPLEX_STREAMED)  ──ext_proc gRPC──►  extproc-guardrails (host :9000)
        │                                                              API :9080  metrics :9090
        ▼
 test upstream (host :8890)  — modes:
   • echo   (default): reflect the masked text back verbatim  → demask round-trip oracle
   • tool   (X-Echo-Mode: tool): reflect text into tool-call arguments
   • proxy  (X-Upstream-Mode: proxy): forward to the REAL model at $REAL_MODEL_URL (:8881)
 and always CAPTURE the post-masking body Envoy forwarded → masking oracle (capture/requests.jsonl)
```

## Prerequisites

- Go (to build the processor and the upstream), Python 3, Docker (for Envoy), `curl`.
- Optional: an OpenAI-compatible model on `http://localhost:8881` for the
  real-model suite (e.g. a `kubectl port-forward`). Override with `REAL_MODEL_URL`.

## Quick start

```sh
cd tests/e2e
./run.sh up          # build binaries, start upstream + processor + Envoy, seed a custom rule
./run.sh test        # deterministic suites: base, expanded, tool-call, structured, config
./run.sh test-real   # real-model suite (needs a model at :8881)
./run.sh down        # stop everything
```

Or everything at once:

```sh
./run.sh all         # up + all deterministic suites + real-model (if :8881 is reachable)
```

### Fix → retest loop

After changing processor code:

```sh
./run.sh restart     # rebuild + restart only the processor (re-seeds the custom rule)
./run.sh test
```

## What each suite checks

| Suite | Script | Verifies |
|---|---|---|
| base | `driver.py plan_base.jsonl` | every dataset entry × 3 APIs × {stream,non-stream}: **round-trip = mask⁻¹**, no secret leaked upstream, no placeholder leaked to client |
| expanded | `driver.py plan_expanded.jsonl` | valid-format keys, valid-checksum PII, dedup, unicode, **1-rune SSE fragmentation**, negatives, placeholder-collision |
| tool-call | `tool_test.py` | tool arguments demask to valid JSON incl. values with `"`/`\` (all 3 APIs, stream+non-stream) |
| structured | `struct_test.py` | multi-field masking: system, tool_use.input, tool_result, instructions, input items, function_call_output (string+array), chat multi/tool_calls |
| real-model | `real_model_test.py` | full pipeline against a live LLM: masking on request, valid framing, no placeholder leak |
| config/behavior | `config_test.py` | API auth, rules CRUD, `setenabled` on a built-in, override-header narrowing (narrow/none/garbage), detect mode, audit (never stores originals) |

Results land in `results/*.json`. The upstream's per-request capture is
`capture/requests.jsonl`.

### Verification is content-safe

The driver never prints raw secrets. It proves `demask(mask(x)) == x` by hashing,
and proves *no upstream leak* via **placeholder-anchored secret recovery** (split
the masked text on placeholder tokens, align the shared context against the
original, and confirm the exact recovered secret bytes are absent from what the
upstream received).

## Individual scripts

```sh
python3 gen_base_plan.py [dataset.jsonl] [out.jsonl]   # dataset → base plan (all APIs × modes)
python3 gen_expanded.py                                # writes tests/dataset/guardrails_dataset_expanded.jsonl + plan_expanded.jsonl
python3 driver.py <plan.jsonl> [--out r.json] [--verbose]
python3 tool_test.py | struct_test.py | real_model_test.py | config_test.py
python3 analyze_leak2.py                               # classify any driver leak flags: real vs artifact
```

Handy env: `GW` (gateway, default `http://localhost:10000`), `CAPTURE_FILE`,
`REAL_MODEL_URL`, `GUARDRAILS_API_TOKEN` (default `e2e-secret-token`).

## Notes

- The processor uses the `in_memory` store, so custom rules/settings reset on
  every restart — `run.sh` re-seeds the `test.quote_token` custom rule (needed by
  the tool-call quote case) after each start.
- The processor is started with `GUARDRAILS_API_TOKEN=e2e-secret-token`,
  audit + masked-text storage on, and the triggered-rules header exposed.
- `proxy-config.yaml` forwards the `x-guardrails-data-types` override header to
  exercise narrowing — production gateways MUST strip the client's copy.
