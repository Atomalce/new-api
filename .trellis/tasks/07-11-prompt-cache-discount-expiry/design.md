# Design: Codex Responses prompt cache discount expiry billing

## Status

Realigned 2026-07-11: scope is narrowed to positively identified Codex traffic
on the exact `/v1/responses` client path with a fixed 180-second cycle. The
former non-Claude-upstream -> `/v1/messages` streaming question is moot because
that client path is out of scope. User decision (2026-07-11): "同一个请求"
means the Codex cache lineage (`prompt_cache_key`, then `Session_id`), not an
exact request-body fingerprint. User decision (2026-07-11): the policy is
always on for eligible traffic with no feature flag. User decision
(2026-07-10): Q3 resolved as compatibility no-op — without Redis the process
starts with a prominent warning and the policy is inactive. Planning is
converged; task is cleared for activation.

## Problem boundary

The gateway cannot evict an upstream provider's physical prompt cache. This is
a local accounting policy:

- every 180 seconds, revoke the cache-read discount once per Codex logical
  cache lineage on `/v1/responses` traffic;
- reclassify that owner request's upstream cache-read tokens as normal input;
- use the adjusted usage consistently for the client response, billing
  settlement, and consume logs;
- retain the original upstream usage for administrator audit and telemetry.

## End-to-end data flow

```text
/v1/responses request
  -> positively classify Codex client (path alone is insufficient)
  -> resolve policy + Codex cache lineage (prompt_cache_key > Session_id)
  -> send upstream (normal retry/channel selection)
  -> parse real upstream usage; verify the protocol claim point
  -> deep-copy immutable raw usage before any mutation
  -> normalize provider cache aliases into a working copy
  -> atomically claim/reuse the request's 180s cycle decision (Redis SETNX)
  -> owner: zero cache-read aliases, input total unchanged
  -> patch the authoritative body / terminal response.* SSE event before flush
  -> return the same adjusted dto.Usage from the handler
  -> ratio / tiered_expr settlement consumes adjusted usage
  -> consume log stores adjusted public fields + admin-only raw audit
```

The adjustment boundary is after provider usage normalization and before any
authoritative usage body or SSE event is flushed. Adjusting only in
`PostTextConsumeQuota` is too late: handlers have already sent client-visible
usage by then.

## Policy activation (selected)

```text
PromptCacheDiscountExpirySetting
  enabled = true     # always on; no feature flag or runtime switch
  ttl_seconds = 180  # fixed; min = max
  rule: exact path /v1/responses + positive Codex classifier, all models
  identity_sources: prompt_cache_key + Codex Session_id
```

Validation and activation:

- the policy is always on for eligible traffic and has no enabled/disabled
  configuration branch;
- a Redis-backed process requires an explicitly configured shared `CRYPTO_SECRET` or
  `SESSION_SECRET` (process-random default rejected) and healthy Redis;
- activation runs after `common.InitRedisClient()` succeeds and before
  `perfmetrics.Init()`/HTTP server start: validate snapshot, bounded Redis
  Ping, then atomically create-or-compare a persistent, policy-version-scoped
  secret-fingerprint sentinel (first node writes; same-secret nodes pass;
  different-secret nodes fail startup). The sentinel has no TTL, so a
  long-running old node cannot outlive the mismatch guard; intentional secret
  rotation requires a full shutdown followed by explicit sentinel deletion;
- there is no environment feature flag, OptionMap, Option/JSON, controller
  endpoint, frontend, or kill switch. The fixed 180-second interval is not
  configurable. Rollback requires deploying code that removes/disables the
  policy; Redis keys then expire naturally.
- Q3 (resolved 2026-07-10): a process with no Redis configured starts in
  compatibility no-op mode with a prominent warning; the policy is inactive
  and usage/billing/response are unchanged. Runtime Redis claim errors remain
  fail-open and request-sticky.

## Codex eligibility

`/v1/responses` is a generic client endpoint, so path matching alone would also
change non-Codex callers. Eligibility is resolved before identity and before any
Redis access:

1. Exact client path is `/v1/responses` (not `/v1/responses/compact`).
2. An incoming Codex signal is present. Known repository values include an
   `Originator` header containing `codex` case-insensitively (`Codex CLI`,
   `codex_cli_rs`) or a non-empty `Session_id` header.
3. The request has a valid Codex cache-lineage identity.

`prompt_cache_key` is an identity candidate, not sufficient Codex evidence by
itself. The classifier is only a product-scope gate, not an authentication or
authorization boundary.

## Same-request identity (selected)

Key material, HMAC-SHA256 with the shared server secret (store digest only):

```text
policy version + user id + client path (/v1/responses)
+ origin model + semantic identity type + logical cache id
```

Logical cache-lineage resolution:

1. Request-body `prompt_cache_key`.
2. Request-header `Session_id` when the body field is absent.
3. Both present and different: the policy chooses the body value. Existing
   field-sync code preserves an already supplied body value rather than
   overwriting it, but does not itself define the billing identity. Both
   sources normalize to semantic type `codex_cache_lineage`; the extraction
   source is audit-only metadata and never splits cycles.
4. Neither present: skip the policy (usage/billing/response unchanged) and
   record an admin-only `missing_identity` diagnostic.

`Originator` classifies the Codex client family but is not an identity.
`previous_response_id` advances per turn and is unsuitable. Exact request-body
hashing is explicitly rejected: normal Codex turns change their JSON suffix
while retaining the same cache lineage. Do not implement a body-hash fallback.

The key intentionally excludes channel id, credential index, retry index,
token id, group, and upstream model: routing changes stay inside the same
user-visible cycle. Different users are always isolated.

## Fixed-cycle state machine

Dedicated Redis namespace, atomic Lua/SETNX-style claim, TTL 180s:

```text
key absent:                    SET key request_id EX 180 -> owner=true
key owner == this request_id:  owner=true   (replay idempotent)
key exists, other owner:       owner=false  (keep discount)
```

- Discounted hits never refresh the TTL.
- A natural upstream miss (zero cache reads) still opens the cycle.
- Claim points (real upstream usage only; local estimates, missing usage,
  HTTP 200 error payloads, and Responses `incomplete`/`failed` never claim):
  - native Responses non-stream: after a protocol-successful 2xx `completed`
    response with upstream usage;
  - native Responses stream: immediately before the terminal usage-bearing
    `response.*` event is emitted;
  - Chat->Responses conversion: immediately before the first upstream-authored
    usage is projected into the Responses payload/event;
  - Gemini Responses: immediately before the first real `usageMetadata` is
    converted; the later synthesized terminal event reuses the decision.
- Post-claim protocol errors, abnormal EOF, or client disconnect do not roll
  back the claim and must not trigger another retry attempt.

`cachex.HybridCache` is unsuitable: no atomic cross-node claim, and its
Set-with-TTL refreshes expiry.

## Canonical usage contract

Request-scoped state on `RelayInfo`: resolved rule, identity digest, accounting
mode, claim result, authoritative-usage marker, original usage snapshot,
adjusted snapshot/audit, applied marker. The decision is request-sticky:
claim success, claim failure, and Redis error are all reused by later events
without another Redis call.

Client projection is always OpenAI Responses included-in-input semantics:

```text
input_tokens total: unchanged
input_tokens_details.cached_tokens and all cache-read aliases: -> 0
output/reasoning/cache-creation dimensions: unchanged
```

Concrete contract:

```text
upstream: input=10000, cached=8000, output=500
owner:   input=10000, cached=0,    output=500
in-cycle non-owner: unchanged upstream usage
```

For the owner, canonical `dto.Usage.PromptTokens` stays 10,000 while every
canonical/cache-read alias used by billing is zeroed, including
`PromptTokensDetails.CachedTokens`, `InputTokensDetails.CachedTokens` when
present, and `PromptCacheHitTokens`. The client-visible Responses path
`usage.input_tokens_details.cached_tokens` is patched to zero before emission.

Converted upstreams carry an explicit accounting mode so a
separate-from-input source (normalized upstream usage) is reclassified exactly
once before projection; converters never claim or adjust again.

Implementation note (2026-07-11): every handler family able to serve
`/v1/responses` (native, Chat->Responses, Gemini) produces included-in-input
OpenAI-semantic usage, so the accounting-mode guard is a validation gate:
usage carrying anthropic semantics or Claude 5m/1h cache-creation splits is
skipped (`invalid_usage`, no claim, admin-audited) instead of reclassified.
Zeroing cache reads on separate-from-input usage would silently drop them
from the bill, so skipping is the billing-safe direction for a branch that is
unreachable today.

Validation precedes the Redis claim: negative, malformed, or overflow-prone
counts skip the policy (no claim, upstream handling preserved) with a warning
and admin-only diagnostic. No negative values, wraparound, or unaudited
saturation; use `common/quota_math.go` helpers for any arithmetic that could
saturate. The mutator is idempotent across repeated stream events.

## Response projection and byte preservation

Adjusted canonical usage is the source of truth; pass-through bodies use
targeted JSON/SSE path patches so unknown provider fields survive and absent
zero-valued fields are not synthesized.

### Handler inventory (client path `/v1/responses`)

| Upstream family | Handlers | Boundary notes |
| --- | --- | --- |
| native Responses | `OaiResponsesHandler`, `OaiResponsesStreamHandler` (`relay/channel/openai/relay_responses.go`) | non-stream currently writes the body before usage construction — reorder to parse -> adjust -> patch -> send; stream unmarshals before emit, patch every terminal `response.*` event carrying usage (not only `response.completed`) |
| Chat -> Responses | handlers in `relay/channel/openai/responses_via_chat.go` | re-marshal path: adjust normalized usage before projection; provider chat aliases (DeepSeek/Moonshot/etc.) are handled by existing normalization before the decision |
| Gemini Responses | `GeminiResponsesHandler` + stream (`relay/channel/gemini/relay_responses.go`) | adjust before `usageMetadata` is converted; terminal synthesis only projects the stored decision |

A source inventory pass confirms this list before integration and adds any
other handler able to serve `/v1/responses`. Cross-channel retries must not
bypass the policy because a different upstream family served the final attempt.
All non-Codex `/v1/responses` requests and all other client paths
(chat/completions, messages, responses/compact, images, realtime, native
Gemini, legacy completions) are explicit policy no-ops in eligibility
resolution, even though the rule may match all models.

Patch coverage vs claim eligibility: `incomplete`/`failed` terminals never
claim; if a claim already happened earlier in the stream, their usage fields
are still patched for client consistency.

Passthrough settings never bypass the active policy, and no usage event is
invented when the client did not request one; the internal adjusted usage
still drives settlement.

## Billing and logging

- Included-in-input adjustment means existing ratio and `tiered_expr` billing
  charge the full input at normal price with no expiry-specific branch;
  `tiered_expr` `len` keeps the original complete context length; fixed
  per-call pricing is charge-invariant.
- For the numeric contract above, legacy ratio settlement sees
  `PromptTokens=10000` and `CachedTokens=0`. `tiered_expr` sees `p=10000`,
  `cr=0`, and `len=10000`. This exact tuple must be asserted rather than only
  checking that a cache field became zero.
- Deep-copy the raw parsed usage (nested token-detail pointers and provider
  extension fields included) before normalization/mutation; keep a distinct
  normalized pre-adjustment snapshot for audit and inference consumers.
- `CalcOpenRouterCacheCreateTokens` (reachable via Chat->Responses conversion
  on an OpenRouter channel) consumes the normalized pre-adjustment snapshot,
  never the adjusted owner snapshot.
- Channel-affinity cache telemetry observes original upstream values (see
  research-merged-code-anchors.md finding 6: restore a 5-field snapshot, not
  just the three cache signals).
- Consume-log public fields carry adjusted values; add an admin-only audit
  object `admin_info.prompt_cache_discount_expiry` with rule, mode, identity
  digest prefix, owner request id, ttl, reason, original/adjusted input and
  cache counts. Never log raw identity values or prompt content.

## Redis and failure behavior

- Shared Redis plus the explicit shared secret is the only way to guarantee one
  cycle owner across nodes. Without Redis the process starts in compatibility
  no-op mode (Q3 resolved); there is no process-local fallback because it
  violates the global owner contract.
- The secret-fingerprint sentinel is persistent rather than time-limited.
  Expiring it would allow a differently configured node to join after the TTL
  while old nodes are still serving and would split one logical cycle.
- Runtime claim timeout/error fails open: preserve upstream usage and
  discount, attach an admin diagnostic, emit a request-correlated warning.
  Request-sticky: later events never retry or flip the decision.
- Never convert cache reads to full price merely because Redis failed.
- Redis flush/eviction/restart can cause an early new cycle (an early
  full-price request); acceptable. Deployments needing stronger guarantees use
  durable no-eviction Redis policy plus health-based traffic gating.

## Tests

Deterministic tests cover:

- reclassification correctness, idempotency, overflow/invalid-input skip, and
  cache-creation preservation;
- atomic first-owner, replay, concurrent boundary, fixed-TTL no-refresh;
- non-Codex Responses, path-ineligible (chat/messages/compact), and
  identity-missing no-ops;
- lineage identity precedence, body-wins conflict, one semantic type,
  source-switch stability, and key-scope separation/non-separation matrices;
- Redis failure fail-open and request-stickiness;
- native Responses stream/non-stream contracts (reordered non-stream send,
  terminal-event patching including `incomplete`/`failed` bodies post-claim);
- Chat->Responses and Gemini Responses conversion contracts, applied exactly
  once, unknown-field preservation;
- claim gating: missing usage, local estimation, pre-claim 200 errors,
  incomplete/failed terminals, post-claim EOF/disconnect no-rollback,
  cross-channel retry;
- legacy ratio + `tiered_expr` full-price settlement with unchanged `len`;
  fixed per-call invariance; include the exact 10000/8000/500 round trip from
  the PRD;
- consume-log adjusted public fields + admin raw/adjusted audit; deep-copy
  isolation under nested mutation;
- OpenRouter cache-creation inference on the pre-adjustment snapshot;
- immutable-policy validation; always-on behavior; configured-Redis startup
  gates (unhealthy Redis, missing/random secret, sentinel
  first/same/different); no-Redis compatibility no-op startup (starts with
  warning, policy inactive); absence of any runtime mutation surface.
