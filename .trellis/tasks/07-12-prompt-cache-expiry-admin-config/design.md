# Design — Admin-configurable prompt cache expiry (toggle + cycle seconds)

## Context

Task 07-11 shipped the policy with a compile-time constant
(`service/prompt_cache_expiry.go`: `PromptCacheExpiryCycleSeconds = 60`,
rule name `codex_responses_60s`) and no runtime switch. This task moves both
knobs into the standard registered-config system so admins control them from
the dashboard.

## Existing infrastructure reused (no new machinery)

- **Registered configs**: `config.GlobalConfig.Register(name, &struct)`
  (pattern: `setting/operation_setting/checkin_setting.go`). Registered
  structs are auto-exported into the option map
  (`model/option.go:180` `ExportAllConfigs`), persisted per dotted key
  (`prompt_cache_expiry_setting.enabled`), loaded at startup
  (`InitOptionMap` → `loadOptionsFromDatabase`), applied on update via
  `handleConfigUpdate` (`model/option.go:580`, reflection-based
  `UpdateConfigFromMap`), and re-synced across nodes by `SyncOptions`.
- **Option API**: existing admin `PUT /api/option/` used by the frontend
  `useUpdateOption` hook. No backend endpoint work.
- **Frontend settings framework**: billing settings page section registry
  (`web/default/src/features/system-settings/billing/section-registry.tsx`),
  defaults map (`billing/index.tsx`), typed dotted keys (`types.ts`).
  `CheckinSettingsSection`
  (`general/checkin-settings-section.tsx`) is the direct template: switch +
  numeric inputs, per-field diffed `updateOption` calls, `form.reset` on save.

Init ordering is already correct: `model.InitOptionMap()` (main.go:332) runs
before `service.InitPromptCacheDiscountExpiry()` (main.go:350), so DB values
are live when the policy initializes and logs.

## Backend design

### New file `setting/operation_setting/prompt_cache_expiry_setting.go`

```go
type PromptCacheExpirySetting struct {
    Enabled      bool `json:"enabled"`
    CycleSeconds int  `json:"cycle_seconds"`
}

var promptCacheExpirySetting = PromptCacheExpirySetting{
    Enabled:      true, // preserve b50eb27a behavior on upgrade
    CycleSeconds: 60,
}

func init() { config.GlobalConfig.Register("prompt_cache_expiry_setting", &promptCacheExpirySetting) }
func GetPromptCacheExpirySetting() *PromptCacheExpirySetting { ... }

// EffectiveCycleSeconds clamps to [10, 86400]; misconfigured values must
// never become a 0/negative Redis TTL or an effectively permanent cycle.
func (s *PromptCacheExpirySetting) EffectiveCycleSeconds() int
```

Clamp bounds are constants in this file
(`PromptCacheExpiryMinCycleSeconds = 10`,
`PromptCacheExpiryMaxCycleSeconds = 86400`); the frontend mirrors the same
numbers in its zod schema. Reads are the same unsynchronized pattern every
other registered setting uses (`quota_setting`, `checkin_setting`); the
existing torn-read window during `UpdateConfigFromMap` is accepted
project-wide and harmless here (worst case: one request claims with the old
TTL or skips one cycle).

### Changes in `service/prompt_cache_expiry.go`

1. **Delete** `PromptCacheExpiryCycleSeconds` (single source of truth becomes
   the setting default). Grep confirms no non-test users outside this file.
2. **Rule name**: `promptCacheExpiryRuleName = "codex_responses_60s"` →
   `"codex_responses"`. TTL was never part of the cycle identity (Redis key =
   prefix + HMAC digest; key material has no TTL), so renaming the audit label
   and changing TTL cannot split or reset in-flight cycles.
3. **Gate** (`resolvePromptCacheExpiryState`, scope-gate branch): add
   `!operation_setting.GetPromptCacheExpirySetting().Enabled` to the existing
   `!promptCacheExpiryActive || ...` condition → `st.Ineligible = true`.
   Eligibility stays resolved once per request (sticky on RelayInfo), so a
   mid-request toggle cannot tear a single request's billing/response/log
   consistency.
4. **Claim TTL** (`ApplyPromptCacheDiscountExpiry`): compute
   `ttl := operation_setting.GetPromptCacheExpirySetting().EffectiveCycleSeconds()`
   immediately before the claim; pass `time.Duration(ttl) * time.Second`;
   record `st.ClaimTTLSeconds = ttl` (new int field on
   `relaycommon.PromptCacheExpiryState`) so the audit reports the TTL actually
   used, not the value configured at log time.
5. **Audit** (`attachPromptCacheExpiryAudit`): `ttl_seconds` =
   `st.ClaimTTLSeconds` when > 0 (a claim happened), else current effective
   value (skip/in-cycle-without-claim paths, informational).
6. **Startup log** (`InitPromptCacheDiscountExpiry`): print
   `enabled=%t ttl=%ds` from the setting instead of the deleted constant.

### Startup validation — unchanged (decision)

`validatePromptCacheExpiryStartup` still runs whenever Redis is enabled,
regardless of `enabled`. Rationale: the toggle can be flipped on at any time
at runtime; infra (secret fingerprint sentinel) must already be validated by
then, and adding a lazy validation path on first-enable is new machinery with
its own failure modes. Known limitation carried over: Redis deployments must
set an explicit shared secret even if they keep the policy disabled.

### Cross-node semantics

`enabled`/`cycle_seconds` propagate via option sync; nodes may disagree for
up to one sync period. TTL disagreement only affects newly opened cycles
(claim is a single `SET NX EX`); enable/disable disagreement means one node
may claim a cycle another node ignores — same fail-open family of outcomes
the policy already tolerates (Redis errors), acceptable during propagation.

## Frontend design

New section on the billing settings page, cloned from the check-in section:

- `general/prompt-cache-expiry-settings-section.tsx`: `SettingsSection` with
  an enable `Switch` + `cycle_seconds` number `Input`
  (`z.coerce.number().int().min(10).max(86400)`), per-field diffed
  `updateOption` writes (keys `prompt_cache_expiry_setting.enabled`,
  `prompt_cache_expiry_setting.cycle_seconds`), description noting Codex
  `/v1/responses` scope and the Redis + shared-secret requirement.
- `billing/section-registry.tsx`: new entry `{ id: 'prompt-cache-expiry',
  titleKey: 'Prompt Cache Billing' }` after `checkin`.
- `billing/index.tsx`: defaults `'prompt_cache_expiry_setting.enabled': true`,
  `'prompt_cache_expiry_setting.cycle_seconds': 60`.
- `types.ts`: the two dotted keys on `BillingSettings`.
- i18n: new English keys + translations in all 6 locale files via
  `bun run i18n:sync` then manual translation fill.

## Testing

- `service/prompt_cache_expiry_test.go`:
  - disabled gate: `enabled=false` → eligible Codex request stays
    passthrough (no claim call, usage untouched), mirroring existing
    inactive-policy tests; restore setting via `t.Cleanup`.
  - TTL: claim receives the configured value (inject via
    `promptCacheExpiryClaimFunc` capture); clamp cases 0/-5/999999 →
    10/10/86400.
  - audit: rule name assertion updated; `ttl_seconds` equals the TTL captured
    by the fake claim.
- Existing suites (service + openai/gemini relay + redis) must pass unchanged
  apart from the rule-name string.
- Frontend: `bun run build` (or the project's check script) + i18n sync
  verification. No new frontend unit test: the section is a config-form clone
  with zod-schema validation, same coverage class as the check-in section.

## Rollout / rollback

- Pure additive option keys; absent rows fall back to in-code defaults that
  reproduce today's behavior. No migration.
- Rollback = revert the commit; leftover option rows for an unregistered
  config name are ignored by `handleConfigUpdate` (returns false, harmless
  legacy keys in the options table — same shape as other retired settings).
