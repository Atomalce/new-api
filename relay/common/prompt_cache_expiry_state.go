package common

import "github.com/QuantumNous/new-api/dto"

// PromptCacheExpiryDecision is the sticky per-request outcome of the Redis
// cycle claim for the prompt-cache discount expiry policy.
type PromptCacheExpiryDecision int

const (
	// PromptCacheExpiryPending means no claim has been attempted yet.
	PromptCacheExpiryPending PromptCacheExpiryDecision = iota
	// PromptCacheExpiryOwner means this request opened the current 180s cycle:
	// its cache-read discount is revoked.
	PromptCacheExpiryOwner
	// PromptCacheExpiryInCycle means another request owns the active cycle:
	// the upstream discount is preserved.
	PromptCacheExpiryInCycle
	// PromptCacheExpiryFailOpen means the Redis claim errored: the upstream
	// discount is preserved and no further claim is attempted for this request.
	PromptCacheExpiryFailOpen
)

const (
	// PromptCacheExpiryAccountingIncludedInInput means cache-read tokens are
	// already included in the reported input total. Revoking the discount only
	// zeroes the cache-read aliases.
	PromptCacheExpiryAccountingIncludedInInput = "included_in_input"
	// PromptCacheExpiryAccountingSeparateFromInput means cache reads would
	// need to be added to input before zeroing. The current Responses policy
	// rejects this mode before claiming rather than risk underbilling.
	PromptCacheExpiryAccountingSeparateFromInput = "separate_from_input"
)

// PromptCacheExpiryState is the request-scoped state of the Codex
// /v1/responses prompt-cache discount expiry policy. It is resolved lazily on
// first use and reused by every later stream event and retry attempt, so the
// billing decision, the client-visible usage, and the consume log always agree.
type PromptCacheExpiryState struct {
	// Ineligible marks requests outside the policy scope (non-Responses path,
	// non-Codex caller, policy inactive). Complete no-op, no audit emitted.
	Ineligible bool
	// SkipReason marks eligible Codex requests the policy skipped
	// (missing_identity, invalid_usage). Surfaced as an admin-only diagnostic.
	SkipReason string

	// IdentityDigest is the HMAC-SHA256 of the policy key material; raw
	// identity values are never stored.
	IdentityDigest string
	// IdentitySource records where the logical cache id came from
	// ("prompt_cache_key" or "session_id"). Audit metadata only; it never
	// participates in the digest.
	IdentitySource string
	// AccountingMode records how cache reads relate to the input total. It is
	// carried on RelayInfo across provider conversions and included in the
	// admin audit so settlement never infers semantics from a projected shape.
	AccountingMode string

	Decision PromptCacheExpiryDecision
	// OwnerRequestId is the request id stored in Redis for the active cycle.
	OwnerRequestId string

	// OriginalUsage is an immutable pre-adjustment deep copy of the usage the
	// owner reclassified, kept for admin audit and raw cache telemetry.
	OriginalUsage *dto.Usage
	// RevokedCacheTokens is the number of cache-read tokens reclassified as
	// full-price input on the owner request.
	RevokedCacheTokens int
	// Adjusted is set once the reclassification has been applied to at least
	// one canonical usage object.
	Adjusted bool
}
