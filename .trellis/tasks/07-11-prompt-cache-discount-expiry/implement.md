# Implementation plan: Codex Responses prompt cache discount expiry billing

## Gate

Cleared 2026-07-10: the user approved the realigned (2026-07-11) PRD/design
and resolved Q3 as compatibility no-op (no Redis -> start with prominent
warning, policy inactive). Scope, fixed 180-second timing, always-on
enablement, and Codex cache-lineage identity (`prompt_cache_key` >
`Session_id`) are confirmed. Implementation may proceed after `task.py start`.

## Ordered checklist

1. PRD convergence (done 2026-07-10).
   - Scope: positively identified Codex traffic on exact `/v1/responses` only;
     non-Codex Responses and every other path are explicit eligibility no-ops.
   - Cycle: fixed 180 seconds; discounted hits never refresh; one owner per
     boundary.
   - Identity: `prompt_cache_key` > `Session_id`, body wins conflicts, one
     `codex_cache_lineage` semantic type; missing identity skips; exact-body
     hashing and automatic fallback are out of scope.
   - Policy is always on with no feature flag or runtime switch; the interval
     remains fixed at 180 seconds and has no 180-300 range.
   - Q3 resolved: no Redis -> compatibility no-op with prominent warning.
     Runtime claim errors remain fail-open and request-sticky.

2. Add the policy definition and startup activation.
   - Internal policy: ttl 180 (min=max), one all-model rule requiring exact
     `/v1/responses` plus positive Codex classification, and the selected
     cache-lineage identity algorithm.
   - Policy is unconditionally active for eligible requests. When Redis is
     configured, initialize after `common.InitRedisClient()` and before
     `perfmetrics.Init()`/server start: snapshot validation, explicit shared
     `CRYPTO_SECRET`/`SESSION_SECRET` check, bounded Redis Ping, and a
     persistent policy-version secret-fingerprint sentinel (atomic
     create-or-compare; mismatch fails startup; secret rotation requires full
     shutdown plus explicit sentinel deletion).
   - No-Redis branch (Q3 resolved): start normally, log a prominent warning,
     policy inactive; no feature flag, OptionMap entry, UI, or configurable
     interval.
   - Tests: invalid snapshot values, path match/non-match, always-on behavior,
     configured Redis with unhealthy/mismatched state, and no-Redis
     compatibility no-op startup.

3. Implement Codex eligibility and cache-lineage identity resolution.
   - Classify before Redis access. Path match alone and `prompt_cache_key`
     alone are insufficient; use explicit incoming Codex signals such as
     `Originator` containing `codex` (case-insensitive) or `Session_id`.
   - Extract non-empty string `prompt_cache_key` from the original request and
     `Session_id` from headers; apply precedence and conflict rules from design.
     Do not hash the complete request body or add a fallback identity.
   - Compose key material (policy version, user id, path, origin model,
     semantic type, logical id) and HMAC with the shared secret; expose only a
     short audit prefix.
   - Admin-only `missing_identity` diagnostic without logging raw values.
   - Tests: Codex/non-Codex classification, precedence, conflict,
     source-switch stability, changed-body/same-lineage stability, missing
     sources, and non-disclosure.

4. Implement the atomic cycle-claim primitive.
   - Dedicated Redis Lua claim with request-id replay idempotency and 180s TTL;
     an existing claim is read without refreshing its TTL. Keep the primitive
     domain-specific in `common/redis.go` rather than exposing a generic
     SetNX wrapper (see research-merged-code-anchors.md finding 10 for the
     original code anchor).
   - Injected clock/TTL for deterministic tests; no-Redis behavior follows Q3.
   - Tests: first-owner, replay, one winner under concurrency (no sleeps).

5. Add request-scoped policy state to `RelayInfo`.
   - Resolved rule, identity digest, accounting mode, claim result,
     authoritative-usage marker, original usage snapshot, adjusted audit,
     applied marker.
   - Request-sticky decision: claim success/failure/Redis-error reused by all
     later events without another Redis call. Beware per-attempt vs
     cross-attempt state on retries (anchors finding 5: retry loop
     controller/relay.go:191-237 leaves RelayInfo fields behind).
   - Claim only at protocol-defined points with real upstream usage; local
     estimates, missing usage, pre-claim 200 errors, `incomplete`/`failed`
     never claim; post-claim failures never roll back or retry.

6. Implement the reclassification mutator.
   - Deep-copy raw usage (nested pointers + provider extensions) before any
     mutation; keep a normalized pre-adjustment snapshot.
   - Included-in-input projection: input total unchanged, all cache-read
     aliases zeroed; explicit accounting mode for converted upstreams so a
     separate-from-input source is reclassified exactly once.
   - Assert the canonical example exactly: upstream
     `{input=10000,cached=8000,output=500}` -> owner
     `{input=10000,cached=0,output=500}`; an in-cycle non-owner is unchanged.
   - Validate before claim; skip on negative/malformed/overflow-prone counts
     with warning + admin diagnostic; `common/quota_math.go` for any
     saturating arithmetic; idempotent under repeated events.
   - Tests: correctness, idempotency, deep-copy isolation, invalid-input skip,
     cache-creation preservation.

7. Inventory `/v1/responses` handlers.
   - Confirm the design table: `relay/channel/openai/relay_responses.go`
     (native, stream + non-stream), `responses_via_chat.go`
     (Chat->Responses), `relay/channel/gemini/relay_responses.go` (Gemini
     Responses). Add anything else able to serve the path.
   - Verify cross-channel retries cannot bypass the policy via a different
     final upstream family.
   - Register non-Codex `/v1/responses` and all other client paths as explicit
     eligibility no-ops.

8. Integrate native Responses paths.
   - Non-stream: reorder to parse -> adjust -> patch -> send (current code
     writes the body at relay_responses.go:44 before usage construction at
     :47 — anchors finding 13).
   - Stream: patch every terminal `response.*` event carrying usage before
     emit (unmarshal at :86 precedes emit at :91); `incomplete`/`failed`
     never claim but are patched post-claim for consistency.
   - Preserve unknown fields; never invent usage events.

9. Integrate Chat->Responses conversion.
   - Adjust the normalized usage once before projection into the Responses
     payload/events; the converter never claims or adjusts again.
   - Provider chat aliases (DeepSeek/Moonshot/Zhipu/llama.cpp) are consumed by
     existing normalization before the decision point; client-visible patching
     targets Responses-format fields only.

10. Integrate Gemini Responses.
    - Adjust before the first real `usageMetadata` is converted; stream claim
      at that boundary; the synthesized terminal event projects the stored
      decision.

11. Integrate settlement, telemetry, and logs.
    - Adjusted usage flows unchanged into legacy ratio and `tiered_expr`
      billing; `len` unchanged; fixed per-call invariant.
    - Assert owner settlement inputs exactly: legacy ratio gets
      `PromptTokens=10000`, `CachedTokens=0`; `tiered_expr` gets `p=10000`,
      `cr=0`, `len=10000` for the PRD example.
    - Channel-affinity telemetry observes original values (restore the 5-field
      snapshot — anchors finding 6).
    - `CalcOpenRouterCacheCreateTokens` gets the normalized pre-adjustment
      snapshot.
    - Admin-only `admin_info.prompt_cache_discount_expiry` audit plus
      request-correlated warnings; verify pre-consume, settlement delta, and
      saturation-audit behavior.

12. Verification and review.
    - Focused package tests: policy/config, identity, Redis claim, each
      handler family, conversion, billing, logs.
    - Claim-gating matrix: missing usage, local fallback, pre-claim 200
      errors, incomplete/failed, post-claim EOF/disconnect, cross-channel
      retry.
    - Startup matrix: Q3-selected no-Redis behavior; configured Redis with a
      random/missing secret, unhealthy connection, and sentinel
      first/same/different cases.
    - Run backend lint/type/test commands; run `trellis-check`; cross-layer
      review of response -> billing -> log consistency.

## High-risk points

- Native Responses non-stream reorder: body currently flushed before usage is
  built; getting the order wrong leaks original usage to the client.
- Stream terminal-event coverage: patching only `response.completed` misses
  usage-bearing `response.done`/`response.incomplete` (anchors finding 12).
- Conversion double-application: adjust before projection exactly once.
- Redis claim atomicity and no-TTL-refresh: anything else changes charge
  frequency.
- OpenRouter cache-creation inference must keep reading pre-adjustment usage.

## Planned validation commands

```text
go test ./service -run 'PromptCache|Tiered|TextQuota'
go test ./relay/channel/openai -run 'Responses|Usage|Cache|Stream'
go test ./relay/channel/gemini -run 'Responses|Usage|Cache'
go test ./service/relayconvert -run 'Usage|Responses'
go test ./common ./pkg/cachex
go test ./...
```

## Execution progress (2026-07-11)

- Implemented the fixed 180-second Redis claim, persistent shared-secret
  sentinel, structured/HMAC cache-lineage identity, request-sticky state,
  usage validation, audit data, and no-Redis compatibility mode.
- Integrated native Responses, Chat-to-Responses, and Gemini Responses for
  streaming and non-streaming. Canonical billing usage is now distinct from
  the client Responses projection; `response.done` and post-claim incomplete
  events are covered.
- Preserved cache-creation/media/reasoning dimensions and routed OpenRouter
  cache-creation inference through the pre-adjustment usage snapshot.
- Added deterministic Redis Lua concurrency/replay/TTL tests plus handler,
  identity, validation, startup, billing, audit, and round-trip regressions.
- Final acceptance hardening added an explicit included-in-input accounting
  mode, combined-total overflow rejection, persistent migration of legacy
  expiring sentinels, and terminal-finish gates so missing `finish_reason` or
  abnormal stream EOF cannot claim a cycle.
- Containerized Go 1.25.1 verification: gofmt check, focused race tests,
  `go build -buildvcs=false ./...`, and `go test ./...` pass. Full
  `go vet ./...` still reports pre-existing lock-copy/IPv6 warnings in
  `common/` and unreachable-code warnings in legacy provider adaptors; vet
  passes for the changed service/relay packages and reports no warning in the
  new Redis code.
