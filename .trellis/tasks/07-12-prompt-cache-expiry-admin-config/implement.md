# Implement — Admin-configurable prompt cache expiry

Ordered checklist. Backend first (independently testable), then frontend.

## 1. Backend setting

- [ ] Add `setting/operation_setting/prompt_cache_expiry_setting.go`:
      struct (`enabled`, `cycle_seconds`), defaults `{true, 60}`, clamp
      constants `[10, 86400]`, `init()` registration as
      `prompt_cache_expiry_setting`, `GetPromptCacheExpirySetting()`,
      `EffectiveCycleSeconds()`.

## 2. Backend policy wiring (`service/prompt_cache_expiry.go`)

- [ ] Delete `PromptCacheExpiryCycleSeconds`; rule name →
      `codex_responses`.
- [ ] Scope gate in `resolvePromptCacheExpiryState`: add
      `!operation_setting.GetPromptCacheExpirySetting().Enabled`.
- [ ] `ApplyPromptCacheDiscountExpiry`: read `EffectiveCycleSeconds()` before
      the claim, use as TTL, store on new field
      `PromptCacheExpiryState.ClaimTTLSeconds` (`relay/common/prompt_cache_expiry_state.go`).
- [ ] `attachPromptCacheExpiryAudit`: `ttl_seconds` from `ClaimTTLSeconds`
      when set, else current effective value.
- [ ] `InitPromptCacheDiscountExpiry` startup log: include
      `enabled=%t ttl=%ds` from the setting.

## 3. Backend tests

- [ ] Update rule-name assertion (`service/prompt_cache_expiry_test.go:628`).
- [ ] New: disabled gate (eligible request, `enabled=false` → no claim, usage
      untouched; `t.Cleanup` restores).
- [ ] New: claim TTL follows setting; clamp table 0/-5/999999 → 10/10/86400.
- [ ] New: audit `ttl_seconds` == TTL used by the captured claim.
- [ ] Validate: `go build ./... && go vet ./... && go test ./service/... ./relay/... ./common/... ./setting/...`
      (docker golang:1.26.1-alpine per memory: no host Go toolchain).

## 4. Frontend

- [ ] `web/default/src/features/system-settings/general/prompt-cache-expiry-settings-section.tsx`
      cloned from `checkin-settings-section.tsx`: switch + number input
      (int, min 10, max 86400), Redis-requirement description, per-field
      diffed `updateOption`.
- [ ] Register section in `billing/section-registry.tsx`
      (id `prompt-cache-expiry`).
- [ ] Defaults in `billing/index.tsx`; dotted-key types in `types.ts`.
- [ ] i18n: add keys to `web/default/src/i18n/locales/*.json` (6 locales),
      run `bun run i18n:sync` from `web/default/`.
- [ ] Validate: from `web/default/`: `bun install` (if needed),
      `bun run build`; run lint/typecheck script if defined.

## 5. Review gates

- [ ] Billing-safety trace (AGENTS.md invariant): setting → clamp →
      claim TTL only; confirm no path lets `cycle_seconds` become a token
      count or quota multiplier.
- [ ] Per AGENTS.md security/quality triggers: `/verify-change` +
      `/verify-quality` on touched paths (billing-adjacent change).

## Rollback

Single revert; additive option keys are inert once code is reverted.
