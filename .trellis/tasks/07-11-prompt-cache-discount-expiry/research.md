# Research: prompt cache discount expiry

> Scope note (2026-07-11 realignment): the task now covers only positively
> identified Codex traffic on the exact `/v1/responses` client path with a fixed
> 180-second cycle. Findings below
> about `/v1/chat/completions`, `/v1/messages`, Claude native handlers, and
> audio paths are retained as reference but are out of scope; see prd.md.

## Code-confirmed Codex scope facts

- `router/relay-router.go` routes `/v1/responses` as a generic Responses API;
  there is no separate Codex-only client route.
- `dto.OpenAIResponsesRequest` exposes `prompt_cache_key`, while
  `relay/common/relay_info.go` retains the parsed request and clones incoming
  request headers onto `RelayInfo.RequestHeaders`.
- Repository tests and Codex pass-through settings use incoming `Originator`
  values such as `Codex CLI` / `codex_cli_rs` and the `Session_id` header.
- The existing channel-affinity rule named `codex cli trace` matches path,
  model, and `prompt_cache_key`; it does not itself prove that the caller is
  Codex. Therefore this billing policy needs its own explicit Codex eligibility
  check. A standard `prompt_cache_key` alone must not opt a non-Codex Responses
  caller into altered billing.
- "Same request" cannot be answered from code alone. User decision
  (2026-07-11) selects cache lineage (`prompt_cache_key` / `Session_id`). Exact
  original-body fingerprinting is rejected because normal Codex turns change
  their request body while retaining the same cache lineage.

## Official provider semantics

Anthropic's official prompt-caching cookbook states that the default cache TTL
is five minutes and refreshes on each hit; a continuously reused entry can stay
warm indefinitely. It also documents a distinct one-hour TTL.

Source:
https://github.com/anthropics/anthropic-cookbook/blob/main/misc/prompt_caching.ipynb

Therefore the requested full-price request every three to five minutes is a
gateway-side fixed-cycle discount-revocation policy, not exact Claude TTL
emulation. Claude `input_tokens` excludes cache reads and writes, so the chosen
synthetic adjustment adds cache-read tokens to ordinary input while preserving
upstream cache-creation/write usage and its 5m/1h pricing splits.

OpenAI's prompt-caching guide recommends reusing `prompt_cache_key` for requests
that share long common prefixes. The key is a stable lineage/routing signal;
exact prefix matching still determines physical cache reuse.

Source:
https://developers.openai.com/api/docs/guides/prompt-caching.md

## Current response and settlement boundaries

The canonical billing object is `dto.Usage`, but several response adaptors send
client-visible bytes before the current settlement path consumes usage:

- OpenAI Chat non-streaming parses usage and writes the response after provider
  post-processing.
- OpenAI Chat streaming may receive authoritative usage in a final chunk or an
  earlier audio-related chunk.
- OpenAI Responses non-streaming currently forwards the body before returning
  normalized usage; the order must become parse -> adjust -> patch -> send.
- OpenAI Responses streaming currently forwards events before inspecting a
  terminal event; terminal usage must be patched before forwarding.
- Native Claude streaming exposes input/cache usage in `message_start`, before
  later completion or error is known.
- Provider-native handlers such as Gemini independently parse cache-token fields
  and serialize Chat/Responses outputs, so integrating only OpenAI and Claude
  directories would leave cross-channel bypasses.
- Protocol converters transform usage semantics and must project a previously
  adjusted snapshot rather than claiming or adjusting again.

The selected shared boundary is after real upstream usage is parsed and before
any authoritative response body/SSE usage is emitted. Existing settlement then
consumes the same adjusted snapshot; a later billing-only rewrite would be too
late for response consistency.

The source inventory establishes this MVP handler matrix:

- OpenAI-compatible Chat: `relay/channel/openai/relay-openai.go`;
- native Responses and compact: `relay/channel/openai/relay_responses.go` and
  `relay_responses_compact.go`;
- Responses -> Chat and Chat -> Responses:
  `relay/channel/openai/chat_via_responses.go` and
  `responses_via_chat.go`;
- native/converted Claude: `relay/channel/claude/relay-claude.go`;
- Gemini Chat and Responses: `relay/channel/gemini/relay-gemini.go` and
  `relay_responses.go`;
- usage projection in `service/relayconvert/` and `service/convert.go`.

The OpenAI-compatible family currently recognizes standard prompt/input detail
fields, top-level `cached_tokens` and `prompt_cache_hit_tokens`, Moonshot
`choices[].usage.cached_tokens`, and llama.cpp `timings.cache_n`. Claude uses
native cache-read fields in non-stream usage, `message_start`, and
`message_delta`. Gemini uses `usageMetadata.cachedContentTokenCount`. Each
source alias that feeds adjusted billing must also be patched before it is
visible to the client.

Images, Realtime, native Gemini `generateContent`, and legacy Completions use
different usage/settlement contracts. They are explicit policy no-ops for this
MVP rather than accidental omissions hidden behind rules.

## Selected normalization contract

One request-scoped operation:

1. verifies that usage is real upstream usage at a protocol-approved claim
   point, not a local token estimate;
2. deep-copies raw usage and nested token-detail/provider fields;
3. normalizes cache aliases into a separate working snapshot;
4. obtains or reuses one Redis cycle decision;
5. applies one idempotent protocol-aware reclassification;
6. patches client-visible usage before send;
7. returns the same adjusted `dto.Usage` to legacy and dynamic settlement;
8. retains raw and normalized pre-adjustment snapshots for telemetry and admin
   audit.

OpenAI-style included-in-input adjustment:

```text
prompt/input total: unchanged
cached/read tokens: original -> 0
```

Claude-style separate-from-input adjustment:

```text
input tokens: original input + original cache read (checked)
cache_read_input_tokens: original -> 0
```

Output, reasoning/media dimensions, and cache-creation fields remain unchanged.

## Fixed-cycle state model

- Use a dedicated Redis atomic claim per HMAC identity.
- Absent key: store `request_id` with a fixed 180-second TTL and
  return owner.
- Same `request_id`: return owner again without refreshing TTL.
- Different existing owner: preserve the upstream cache-read discount.
- Discounted hits never refresh the cycle.
- A real upstream natural miss can open the cycle so the next hit is not forced
  full price.
- Runtime Redis claim failure is request-sticky fail-open: preserve original
  usage/discount, log an admin diagnostic and warning, and never retry on later
  stream events.
- Enabling requires healthy shared Redis and an explicitly configured shared
  `CRYPTO_SECRET` or `SESSION_SECRET`; process-local state and process-random
  HMAC secrets are invalid.

The existing `cachex.HybridCache` lacks the required cross-node atomic claim and
its normal Set-with-TTL behavior can refresh expiry, so it is not the policy
state primitive.

## Selected identity model

The Redis key is the HMAC-SHA256 of:

```text
policy version
+ user id
+ client protocol / normalized path
+ original model
+ semantic identity type
+ explicit logical cache id
```

The key intentionally excludes channel, provider account/credential index,
retry, upstream routing, user group, API token, and upstream model. These are
physical routing details and must not produce extra full-price cycles for the
same user-visible cache lineage. Different users remain isolated.

Codex `/v1/responses` uses body `prompt_cache_key`, then header `Session_id`;
body wins a conflict. Both extraction paths normalize to semantic type
`codex_cache_lineage`, while the actual source remains audit metadata only.
`Originator` is a client classifier, not an identity. Exact original-body
fingerprinting and automatic fallback hashing are out of scope.

Other header/JSON/session identifiers are eligible only when explicitly defined
as stable cache lineage. If no explicit identity exists, the policy is a no-op
with an admin-only `missing_identity` diagnostic.

Rejected identity alternatives:

- HTTP `request_id`: unique per call, suitable only for owner replay
  idempotency.
- Complete request-body hash: changes across multi-turn suffixes and is not a
  prompt-cache identity contract.
- Cacheable-prefix hash fallback: not selected for the MVP because reliable
  provider-neutral prefix extraction/canonicalization is unavailable.
- Channel/credential/provider-account scope: rejected in favor of the user's
  logical cycle across retries and routing changes.

## Authoritative usage and streaming timing

Local estimation, missing usage, HTTP 200 protocol errors, and Responses
`incomplete`/`failed` terminals must not claim a cycle. Claim points are:

- OpenAI Chat non-stream: a successful response with upstream-authored usage;
- OpenAI Chat stream: immediately before its first upstream-authored usage chunk
  is sent;
- Responses stream/non-stream: a `completed` response with upstream-authored
  usage;
- Claude non-stream: a successful message with upstream-authored usage;
- native Claude stream: immediately before upstream
  `message_start.message.usage` is sent.
- Gemini non-stream: after an unblocked/protocol-successful candidate with real
  `usageMetadata` is available.
- Gemini stream/Responses conversion: immediately before the first real
  `usageMetadata` is emitted or converted; a later synthesized terminal event
  reuses the decision.

Claude streaming cannot wait for final completion without buffering the entire
stream. The chosen behavior treats real `message_start` usage on the selected
2xx stream as the claim point. A later upstream error, abnormal EOF, or client
disconnect does not roll the claim back, and a retry must not start after that
client-visible usage.

The same pre/post-claim distinction applies to every protocol: an HTTP 200 error
payload before real usage does not claim, while an error/EOF/disconnect after a
real usage claim does not roll back and must not trigger another retry.

Non-Claude upstream streams converted to a Claude client have a separate timing
problem: the converter emits a locally estimated `message_start` before the real
upstream cache usage appears near the end. Exact consistency requires either an
explicit policy no-op for those converted streams or whole-stream buffering.
Allowing the early estimate to disagree with final adjusted usage is rejected.

## Selected backend activation model and pending no-Redis behavior

The policy publishes one immutable, always-enabled in-process snapshot during
startup. TTL, model/path scope, and identity sources are code-defined and are
not stored in environment feature flags, OptionMap, database options,
controller APIs, or frontend state. No value changes at runtime; rollback
requires a code deployment.

The current snapshot proposal contains one all-model rule for positively
identified Codex traffic on exact `/v1/responses` only. Every other client path
and non-Codex Responses traffic are no-ops. Identity sources are body
`prompt_cache_key` and the Codex `Session_id` fallback, with identity-missing
requests remaining no-op.

When Redis is configured, startup validates the complete snapshot, requires an
explicitly configured shared `CRYPTO_SECRET` or `SESSION_SECRET`, and performs
a bounded Redis health check after `common.InitRedisClient()` and before
metrics/server startup. It then atomically creates or compares a policy-version
secret-fingerprint sentinel in Redis. The sentinel is persistent: a TTL would
allow a differently configured node to join after expiry while old nodes keep
serving. First and same-secret nodes pass;
different-secret nodes, sentinel errors, the process-random secret fallback,
and unhealthy configured Redis fail process startup. Q3 decides behavior when
Redis is not configured at all.

Runtime Option/JSON configuration is not proposed. The existing option-update
path writes database state before updating memory, while generic config loading
can ignore parse failures; avoiding that path removes partial-update and
cross-node snapshot-consistency concerns. User decision (2026-07-11) selects
always-on behavior with no feature flag. The interval is fixed at 180 seconds.

OpenRouter cache-creation inference is another ordering-sensitive consumer:
`CalcOpenRouterCacheCreateTokens` depends on cached-token values and must receive
the normalized pre-adjustment snapshot. Passing adjusted owner usage would clear
the evidence used for creation inference and change cache-write billing.

## Rejected behavior alternatives

- Sliding TTL: rejected because continuous hits could preserve the discount
  forever.
- Physical upstream cache eviction: unavailable at this response boundary and
  outside the accounting policy.
- Runtime Redis fail-closed or synthetic full-price-on-error: rejected because
  a state outage must not overcharge users.
- Per-process fallback: rejected because it produces multiple owners across
  nodes.
- Retry identity based on the final channel/credential: rejected because those
  fields are excluded from the user-logical key.
