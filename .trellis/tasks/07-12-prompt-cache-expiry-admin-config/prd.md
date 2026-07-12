# Admin-configurable Codex prompt cache expiry policy (toggle + cycle seconds)

## Goal

Let administrators control the Codex `/v1/responses` prompt-cache discount
expiry policy (task `07-11-prompt-cache-discount-expiry`, commit `b50eb27a`)
from the admin dashboard, without code changes or restarts:

1. turn the policy on/off at runtime;
2. set the cycle length (expiry interval) in seconds.

This supersedes the earlier decision (2026-07-11) that the policy is always-on
with a fixed compile-time cycle. User decision (2026-07-12): make both
runtime-configurable via the standard options system.

## Requirements

### R1. Runtime setting

- New registered config `prompt_cache_expiry_setting` with fields:
  - `enabled` (bool) — operator switch for the policy.
  - `cycle_seconds` (int) — cycle length in seconds.
- Defaults preserve current shipped behavior: `enabled=true`,
  `cycle_seconds=60`.
- Stored in the options table, synced across nodes by the existing
  `SyncOptions` loop, editable through the existing option API. No new API
  endpoints.

### R2. Policy gating

- The policy applies only when BOTH hold: infrastructure is ready
  (`promptCacheExpiryActive`: Redis + validated shared secret, unchanged) AND
  `enabled=true`.
- `enabled=false` makes eligible requests behave exactly like the
  pre-`b50eb27a` passthrough: upstream discount preserved, no Redis claim, no
  usage rewrite. Effective from the next request after the option propagates;
  in-flight requests keep their sticky per-request decision.
- Startup validation semantics are unchanged: Redis configured without a valid
  explicit shared secret still fails startup, even when `enabled=false`
  (documented known limitation; avoids a lazy runtime-validation path).

### R3. Cycle length

- The Redis claim TTL uses the configured `cycle_seconds` instead of the
  compile-time constant.
- Safety clamp at read time: values outside [10, 86400] seconds are clamped
  (misconfiguration must never produce a 0/negative TTL or an effectively
  permanent cycle).
- Changing the value affects newly opened cycles only; existing Redis cycle
  keys expire on their old TTL (at most one transition cycle, no cleanup
  needed).
- The consume-log audit (`other.admin_info.prompt_cache_discount_expiry`)
  reports the TTL actually used for the claim, and the rule name no longer
  hardcodes `60s`.

### R4. Admin UI

- New section on the billing settings page (`/system-settings/billing`),
  modeled on the check-in section: an enable switch + a cycle-seconds number
  input (integer, min 10, max 86400).
- Description text states the scope (Codex `/v1/responses` cache-discount
  expiry billing) and that the policy requires Redis plus a shared
  SESSION_SECRET/CRYPTO_SECRET, otherwise it stays inactive even when enabled.
- All user-facing text goes through i18next; keys added to every locale file
  present in the repo (en/zh/zh-TW/fr/ru/ja/vi — 7 files, one more than the
  6 listed in AGENTS.md).

## Acceptance Criteria

- [x] Toggling `enabled` off in the admin UI stops usage rewriting for new
      Codex requests without a restart; toggling on resumes it (given Redis).
- [x] Changing `cycle_seconds` in the admin UI changes the TTL of newly
      claimed cycles without a restart.
- [x] `cycle_seconds` outside [10, 86400] never reaches Redis unclamped
      (backend clamp covers writes bypassing the UI validation).
- [x] Defaults (`enabled=true`, `cycle_seconds=60`) keep upgrade behavior
      identical for deployments that never touch the new settings.
- [x] Audit `ttl_seconds` matches the claim TTL actually used; rule name is no
      longer `codex_responses_60s`.
- [x] Existing prompt-cache-expiry test suites still pass; new tests cover the
      disabled gate and the clamp.
- [x] Frontend builds; new strings present in all 7 locales.

## Non-goals

- No per-user/per-channel/per-model overrides; the setting is global.
- No lazy infra validation when enabling at runtime on a node without
  Redis/secret (stays a no-op with the existing startup warning).
- No changes to identity derivation, claim semantics, fail-open behavior, or
  accounting-mode rules from task 07-11.
