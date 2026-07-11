# Codex Responses prompt cache discount expiry billing

## Goal

Implement a backend-only, gateway-side fixed-cycle policy for Codex
`/v1/responses` traffic that periodically revokes the upstream prompt-cache read
discount. Every 180 seconds (3 minutes), the first eligible request for the
same Codex logical cache lineage becomes the cycle owner: its
upstream-reported cache-read tokens are reclassified as normal full-price
input, that adjusted usage is written to the bill (quota settlement and
consume log), and the client-visible response usage is modified to match. All
other requests inside the active cycle keep the upstream discount unchanged.

This policy changes local accounting and returned usage metadata only. It does
not delete or invalidate the provider's physical prompt cache.

## Scope realignment (2026-07-11, supersedes the earlier four-path scope)

- Only positively identified Codex traffic on the exact `/v1/responses` client
  path is covered. The path alone is not a Codex classifier. Non-Codex
  `/v1/responses` callers, `/v1/chat/completions`, `/v1/messages`,
  `/v1/responses/compact`, and every other client protocol are explicit no-ops.
- The cycle length is fixed at exactly 180 seconds (previously a 180-300 range).
- User decision (2026-07-11): "同一个请求" means the Codex logical cache
  lineage—request-body `prompt_cache_key`, falling back to the `Session_id`
  header—not an exact request-body hash. Consecutive Codex turns with the same
  lineage share one 180-second cycle even when their JSON bodies differ.
- The previous open question (non-Claude upstream streams converted to
  `/v1/messages`) is moot: that client path is out of scope.

## Background

- The repository routes every Responses client through the same
  `/v1/responses` endpoint; it does not provide a separate Codex-only route.
  Incoming `Originator` / `Session_id` headers and the parsed
  `prompt_cache_key` field are available on `RelayInfo`, so eligibility must be
  resolved explicitly before any Redis claim or usage mutation.
- Upstream usage reports cache reads in
  OpenAI-style fields (`input_tokens_details.cached_tokens` and provider
  aliases). OpenAI-style input totals already include cache-read tokens.
- A `/v1/responses` client request may be served by a native Responses
  upstream, a Chat-only upstream converted to Responses, or a Gemini upstream
  projected to Responses. All three families must apply the same decision.
- Usage must be adjusted after provider-specific alias normalization and before
  an authoritative response body or SSE usage event is sent to the client.
- The fixed 3-minute full-price request is a local discount-revocation policy,
  not exact provider TTL emulation.

## Requirements

### R1. Fixed 180-second cycle

- Cycle length is exactly 180 seconds for every identity.
- Discounted requests inside the active cycle do not refresh its expiry.
- The first successful eligible request that atomically opens a new cycle is
  the cycle owner. If it reports cache-read tokens, all of them lose the
  discount and are billed as ordinary input.
- If the owner is a natural upstream miss with zero cache-read tokens, it still
  opens the cycle so the next cache hit is not forced full price again.
- At a concurrent boundary, exactly one distinct request becomes owner across
  all nodes; the others preserve upstream cache-read usage.
- Only real upstream usage is authoritative. Locally estimated fallback usage,
  missing usage, pre-claim HTTP 200 error payloads, and Responses
  `incomplete`/`failed` terminals must not open a cycle. Non-streaming requests
  claim after a protocol-successful final 2xx response with upstream usage;
  streaming requests claim immediately before their first trustworthy usage
  event is emitted or converted. A later protocol error, abnormal EOF, or
  client disconnect does not roll back a completed claim, and no retry may
  start after client-visible usage has claimed.

### R2. Codex eligibility and same-request identity

- A request is eligible only when the client path is exactly `/v1/responses`
  and the request is positively classified as Codex. The implementation must
  use explicit incoming Codex signals (known repository values include an
  `Originator` value containing `codex`, case-insensitive, and the Codex
  `Session_id` header). `prompt_cache_key` alone is a standard Responses field
  and must not classify an otherwise non-Codex caller as Codex.
- Codex classification is an eligibility decision, not the cycle identity.
  Spoofable client headers are acceptable for this product-scoping gate because
  it is not an authorization boundary; user authentication and quota ownership
  remain unchanged.
- State-key material: policy version, user id, client path (`/v1/responses`),
  original model, semantic identity type, logical cache id.
- HMAC-SHA256 the complete material with the shared server crypto secret; store
  only the digest, never raw identity values or prompt content.
- Identity order: non-empty string request-body
  `prompt_cache_key` first; non-empty request-header `Session_id` when the body
  field is absent; if both exist and differ, the body field wins. Both sources
  normalize to the same semantic identity type `codex_cache_lineage`; the
  extraction source is audit metadata only and must not split cycles.
- `Originator` identifies the Codex client family but is not a cache identity.
- Requests without an effective explicit identity skip the policy entirely
  (usage, billing, and response unchanged) with an admin-only diagnostic. Do
  not silently fall back to a request-body fingerprint or another identity
  algorithm after deployment.
- Exclude channel id, upstream credential index, retry index, user group, API
  token id, and upstream routing from the key: routing changes must not create
  additional cycles for the same user and lineage. Different users always have
  independent cycles.

### R3. Responses usage adjustment

- OpenAI Responses included-in-input semantics: keep the total input count
  unchanged and set every client-visible cache-read alias to zero, so the full
  input is billed at the normal input price.
- Exact owner example: if upstream reports `input_tokens=10,000`,
  `input_tokens_details.cached_tokens=8,000`, and `output_tokens=500`, the
  adjusted owner usage remains `input_tokens=10,000` and `output_tokens=500`
  but reports `cached_tokens=0`. Ratio billing therefore prices all 10,000
  input tokens at the normal input rate; `tiered_expr` receives `p=10,000`,
  `cr=0`, and the unchanged `len=10,000`. A non-owner request inside the same
  cycle keeps the upstream `cached_tokens=8,000` and existing discount.
- When the upstream is Chat or Gemini and the response is converted/projected
  to Responses format, adjust the normalized usage once before conversion;
  converters only project the adjusted snapshot and must not claim or adjust
  again. Carry an explicit accounting mode through conversions so upstream
  semantics (e.g. a separate-from-input source) cannot cause double counting.
- Adjustment is idempotent across repeated streaming usage events.
- Validate usage before the Redis claim. Negative, malformed, or
  overflow-prone counts skip the policy for the request (no claim, upstream
  handling preserved) with a request-correlated warning plus admin-only
  diagnostic. The policy must never introduce a negative value, integer
  wraparound, or unaudited saturation.
- Preserve output, reasoning, and unrelated usage dimensions, and preserve
  cache-creation/write totals and prices unchanged.
- Fixed per-call pricing remains charge-invariant even if usage is adjusted.

### R4. One adjusted source of truth

- One request-scoped decision and adjusted usage snapshot drives legacy ratio
  billing, dynamic `tiered_expr` billing, user/channel quota settlement,
  consume-log input/cache fields, and client-visible usage bodies/SSE events.
  No independent billing-only or response-only rewrites.
- Preserve the original upstream usage separately (immutable deep copies,
  including nested token-detail pointers and provider extension fields) for
  administrator audit, raw cache telemetry, and provider-specific
  cache-creation inference (e.g. OpenRouter-served conversions).
- `tiered_expr` context length (`len`) remains the complete original input
  context after reclassification.

### R5. Response coverage on `/v1/responses`

- Cover streaming and non-streaming for every upstream family that can serve
  the path: native Responses handlers, Chat-to-Responses conversion handlers,
  and Gemini Responses handlers. Complete a source inventory before
  integration and add any additional discovered handler.
- Patch authoritative usage before it is flushed: non-streaming bodies must be
  parsed, adjusted, and patched before forwarding; streaming must patch every
  terminal `response.*` event that carries usage (patch coverage is broader
  than claim eligibility: `incomplete`/`failed` payloads are patched for
  consistency only if a claim already happened, and never claim themselves).
- Preserve unknown provider response fields with targeted JSON/SSE patches
  where pass-through fidelity matters; do not synthesize absent fields or
  invent usage events the client did not request.
- Request/response passthrough settings do not bypass the active policy:
  client-visible usage must never disagree with the bill.

### R6. Redis state and failure behavior

- Dedicated atomic Redis Lua/SETNX-style claim with request-id replay
  idempotency and a 180-second key TTL.
- Always-on policy state requires a shared Redis and an explicitly configured
  shared `CRYPTO_SECRET` or `SESSION_SECRET` for cross-node consistency;
  process-random secrets are invalid. All participating nodes share the same
  Redis database and secret; startup uses a policy-version-scoped
  secret-fingerprint sentinel (first node writes, same-secret nodes pass,
  different-secret nodes fail startup). The sentinel is persistent and secret
  rotation requires a full shutdown plus explicit sentinel deletion; it must
  not expire while old nodes may still be serving. When Redis is not
  configured, the process starts in compatibility no-op mode with a prominent
  warning (Q3, resolved below).
- A runtime Redis claim timeout/error fails open for billing: preserve upstream
  usage and discount, return original usage to the client, emit a
  request-correlated warning plus admin audit diagnostic. The failure decision
  is request-sticky; later events in the same response never retry the claim.
- Never convert cache reads to full price merely because Redis failed.

### R7. Activation and observability

- Backend-only; no admin frontend, form, or i18n work.
- The interval is fixed at 180 seconds and is not an operator-configurable
  180-300 range. User decision (2026-07-11): the policy is always on for
  eligible Codex traffic; there is no feature flag, frontend, OptionMap entry,
  runtime API, kill switch, or mutable interval.
- The policy rule must include the strict Codex eligibility gate from R2;
  path match plus `prompt_cache_key` alone is insufficient. Redis is required
  for globally consistent cycle ownership and runtime Redis errors fail open.
- When Redis is configured, startup validates the policy, requires the explicit
  shared secret, and performs a bounded Redis health check plus the sentinel
  check during backend initialization; any validation or mismatch failure
  fails startup. Without Redis, startup succeeds in compatibility no-op mode
  with a prominent warning (Q3 resolved).
- Store an admin-only consume-log audit: rule, identity digest prefix, owner
  request id, TTL, reason, original versus adjusted input/cache counts. Never
  log raw `prompt_cache_key`, `Session_id`, headers, or prompt content.

## Acceptance Criteria

- [x] Non-`/v1/responses` client paths (chat/completions, messages,
      responses/compact, others) are complete no-ops.
- [x] Non-Codex callers on `/v1/responses` are complete no-ops; a standard
      `prompt_cache_key` by itself does not opt a caller into this billing
      policy.
- [x] Continuous discounted Codex traffic sharing one logical cache lineage
      produces exactly one full-price owner at each 180-second boundary;
      discounted hits do not refresh the cycle.
- [x] Exactly one request wins a concurrent boundary across nodes, with replay
      idempotency for the same request id.
- [x] A natural upstream miss opens the next cycle without a second forced
      full-price request.
- [x] Owner usage keeps total input unchanged and reports zero cache read
      consistently to billing, consume logs, and the client (streaming and
      non-streaming).
- [x] Numeric contract: upstream `{input=10000, cached=8000, output=500}`
      becomes owner `{input=10000, cached=0, output=500}` everywhere; the
      corresponding ratio/tiered settlement charges 10,000 normal-price input
      tokens. An in-cycle non-owner preserves `{input=10000, cached=8000}`.
- [x] Cache-creation totals and prices remain unchanged.
- [x] Legacy ratio and `tiered_expr` billing both charge reclassified cache
      reads at normal input price; `len` is unchanged; fixed per-call charge
      is invariant.
- [x] Codex identity is `prompt_cache_key` first, `Session_id` fallback, body
      wins conflicts, and both share `codex_cache_lineage`; switching sources
      with the same logical value does not open another cycle.
- [x] The same logical identity value for different users has independent
      cycles; changing policy version, model, or selected identity opens a
      distinct cycle; changing group, token, channel, credential, retry, or
      routing does not.
- [x] Missing identity is a no-op with an admin-only skip diagnostic.
- [x] Native Responses, Chat-to-Responses, and Gemini Responses handlers
      (stream and non-stream) all emit the same adjusted usage used for
      settlement; converters apply the policy exactly once.
- [x] Unknown provider fields are preserved; no usage event is invented.
- [x] Failed retries, missing usage, local estimation fallback, pre-claim
      HTTP 200 errors, and `incomplete`/`failed` terminals do not claim; a
      completed claim is never rolled back by later stream failure.
- [x] Runtime Redis errors preserve upstream usage/discount, warn admins, and
      are request-sticky.
- [x] Original upstream usage stays available for admin audit and raw cache
      telemetry; deep-copy isolation holds under nested-field mutation.
- [x] Admin audit contains rule, digest prefix, owner request id, TTL, reason,
      and original/adjusted counts; raw identity values never appear in logs
      or responses.
- [x] Invalid, negative, or overflow-prone usage neither claims nor produces
      negative values, wraparound, or unaudited saturation.
- [x] With Redis configured, startup fails on unhealthy Redis, missing/random
      secret, or secret-fingerprint mismatch. Without Redis, startup succeeds,
      logs a prominent warning, and the policy is a complete no-op.
- [x] Deterministic tests cover non-Codex and path no-ops, lineage identity
      behavior, boundary, concurrency, retry, Redis-error, conversion, billing,
      logs, client response round trips, and Q3 startup behavior.

## Out of Scope

- `/v1/chat/completions`, `/v1/messages`, `/v1/responses/compact`, OpenAI
  Images/Realtime/audio, native Gemini `generateContent`, legacy
  `/v1/completions`, and every non-Responses client protocol.
- Exact provider sliding-TTL or cache-write-on-expiry emulation.
- Exact request-body fingerprint identity or automatic fallback to body
  hashing.
- Runtime policy switches, admin UI, OptionMap configuration, or a configurable
  interval.
- Retroactively rewriting historical consume logs.
- Changing provider pricing tables or billing-expression coefficients.

## Resolved product decisions

### Q3. No-Redis startup behavior (resolved 2026-07-10)

User selected **compatibility no-op**: when Redis is not configured, the
server starts normally, logs a prominent warning, and the policy is inactive
(usage/billing/response unchanged) because a single cross-node atomic cycle
decision is impossible. This preserves the project's existing Redis-optional
deployment contract and never overcharges. The fail-startup and process-local
fallback alternatives are rejected.
