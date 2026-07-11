package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex /v1/responses prompt-cache discount expiry policy.
//
// Every 60 seconds, the first positively identified Codex request for the
// same logical cache lineage (request-body prompt_cache_key, falling back to
// the Session_id header) becomes the cycle owner: its upstream-reported
// cache-read tokens are reclassified as normal full-price input. Billing,
// consume logs, and the client-visible response usage are all driven by that
// single request-scoped decision. The policy never touches the provider's
// physical prompt cache.
//
// The policy is always on for eligible traffic and has no runtime switch. It
// requires a shared Redis and an explicitly configured shared secret; without
// Redis the process starts in compatibility no-op mode (user decision, task
// 07-11-prompt-cache-discount-expiry Q3).
const (
	promptCacheExpiryPolicyVersion = "v1"
	promptCacheExpiryRuleName      = "codex_responses_60s"
	promptCacheExpiryIdentityType  = "codex_cache_lineage"
	promptCacheExpiryClientPath    = "/v1/responses"
	// PromptCacheExpiryCycleSeconds is the fixed cycle length. Discounted
	// requests inside the cycle never refresh it.
	PromptCacheExpiryCycleSeconds = 60

	promptCacheExpiryKeyPrefix   = "pce:" + promptCacheExpiryPolicyVersion + ":c:"
	promptCacheExpirySentinelKey = "pce:" + promptCacheExpiryPolicyVersion + ":secret_fp"

	promptCacheExpirySkipMissingIdentity = "missing_identity"
	promptCacheExpirySkipInvalidUsage    = "invalid_usage"
	promptCacheExpirySkipInvalidIdentity = "invalid_identity"
)

var (
	promptCacheExpiryActive bool
	// promptCacheExpiryClaimFunc is the atomic cycle-claim primitive, wired to
	// common.RedisClaimCycleOwner at startup and injectable in tests.
	promptCacheExpiryClaimFunc func(key string, owner string, ttl time.Duration) (bool, string, error)
)

// InitPromptCacheDiscountExpiry validates and activates the policy during
// backend startup, after common.InitRedisClient() and before the HTTP server
// starts. Without Redis the process starts with the policy inactive
// (compatibility no-op); with Redis, a missing explicit shared secret, an
// unhealthy Redis, or a secret-fingerprint mismatch fails startup.
func InitPromptCacheDiscountExpiry() {
	if !common.RedisEnabled {
		common.SysLog("[prompt_cache_expiry] WARNING: Redis is not configured; the Codex /v1/responses prompt-cache discount expiry policy is INACTIVE (compatibility no-op). Configure Redis and a shared SESSION_SECRET/CRYPTO_SECRET on every node to activate it.")
		promptCacheExpiryActive = false
		return
	}
	if err := validatePromptCacheExpiryStartup(); err != nil {
		common.FatalLog("[prompt_cache_expiry] " + err.Error())
	}
	promptCacheExpiryClaimFunc = common.RedisClaimCycleOwner
	promptCacheExpiryActive = true
	common.SysLog(fmt.Sprintf("[prompt_cache_expiry] active: rule=%s ttl=%ds path=%s", promptCacheExpiryRuleName, PromptCacheExpiryCycleSeconds, promptCacheExpiryClientPath))
}

func validatePromptCacheExpiryStartup() error {
	if os.Getenv("CRYPTO_SECRET") == "" && os.Getenv("SESSION_SECRET") == "" {
		return fmt.Errorf("Redis is configured but neither CRYPTO_SECRET nor SESSION_SECRET is set; the policy needs an explicitly shared secret for cross-node cycle identity (process-random secrets would split billing cycles per node)")
	}
	if common.CryptoSecret == "" || common.CryptoSecret == "random_string" {
		return fmt.Errorf("the configured CRYPTO_SECRET/SESSION_SECRET is invalid; set an explicit non-default shared secret on every node")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := common.RDB.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("Redis health check failed: %s", err.Error())
	}
	fingerprint := common.HmacSha256("prompt_cache_discount_expiry:secret_fingerprint:"+promptCacheExpiryPolicyVersion, common.CryptoSecret)
	matched, existing, err := common.RedisEnsurePersistentValue(promptCacheExpirySentinelKey, fingerprint)
	if err != nil {
		return fmt.Errorf("secret-fingerprint sentinel check failed: %s", err.Error())
	}
	if !matched || existing != fingerprint {
		return fmt.Errorf("shared-secret fingerprint mismatch: another node registered a different CRYPTO_SECRET/SESSION_SECRET for this policy version; align the secret on every node, stop all nodes, then delete Redis key %q before restarting", promptCacheExpirySentinelKey)
	}
	return nil
}

// resolvePromptCacheExpiryState resolves eligibility and the cache-lineage
// identity exactly once per request; the result is sticky on RelayInfo.
func resolvePromptCacheExpiryState(info *relaycommon.RelayInfo) *relaycommon.PromptCacheExpiryState {
	if info.PromptCacheExpiry != nil {
		return info.PromptCacheExpiry
	}
	st := &relaycommon.PromptCacheExpiryState{
		AccountingMode: relaycommon.PromptCacheExpiryAccountingIncludedInInput,
	}
	info.PromptCacheExpiry = st

	// Scope gate: only the exact /v1/responses client path (the compaction
	// endpoint uses a distinct relay format) on real, billable requests, and
	// only while the policy is active.
	if !promptCacheExpiryActive || info.RelayFormat != types.RelayFormatOpenAIResponses || info.IsChannelTest {
		st.Ineligible = true
		return st
	}

	// Positive Codex classification: an Originator header containing "codex"
	// (e.g. "Codex CLI", "codex_cli_rs") or a non-empty Session_id header.
	// A standard prompt_cache_key alone never opts a caller in.
	originator := promptCacheExpiryHeader(info.RequestHeaders, "originator")
	sessionId := promptCacheExpiryHeader(info.RequestHeaders, "session_id")
	if !strings.Contains(strings.ToLower(originator), "codex") && sessionId == "" {
		st.Ineligible = true
		return st
	}

	// Cache-lineage identity: body prompt_cache_key wins over Session_id.
	// Both normalize to one semantic identity type so switching sources with
	// the same logical value never splits cycles.
	logicalId := promptCacheKeyFromRequest(info.Request)
	source := "prompt_cache_key"
	if logicalId == "" && sessionId != "" {
		logicalId = sessionId
		source = "session_id"
	}
	if logicalId == "" {
		st.SkipReason = promptCacheExpirySkipMissingIdentity
		return st
	}

	// Key material deliberately excludes channel, credential, retry, token,
	// and group: routing changes must not open extra cycles for the same
	// user and lineage.
	material, err := common.Marshal(struct {
		PolicyVersion string `json:"policy_version"`
		UserId        int    `json:"user_id"`
		ClientPath    string `json:"client_path"`
		OriginModel   string `json:"origin_model"`
		IdentityType  string `json:"identity_type"`
		LogicalId     string `json:"logical_id"`
	}{
		PolicyVersion: promptCacheExpiryPolicyVersion,
		UserId:        info.UserId,
		ClientPath:    promptCacheExpiryClientPath,
		OriginModel:   info.OriginModelName,
		IdentityType:  promptCacheExpiryIdentityType,
		LogicalId:     logicalId,
	})
	if err != nil {
		st.SkipReason = promptCacheExpirySkipInvalidIdentity
		return st
	}
	st.IdentityDigest = common.HmacSha256(string(material), common.CryptoSecret)
	st.IdentitySource = source
	return st
}

func promptCacheExpiryHeader(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func promptCacheKeyFromRequest(request dto.Request) string {
	req, ok := request.(*dto.OpenAIResponsesRequest)
	if !ok || len(req.PromptCacheKey) == 0 {
		return ""
	}
	if common.GetJsonType(req.PromptCacheKey) != "string" {
		return ""
	}
	var key string
	if err := common.Unmarshal(req.PromptCacheKey, &key); err != nil {
		return ""
	}
	return strings.TrimSpace(key)
}

func promptCacheExpiryCachedTokens(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	cached := usage.PromptTokensDetails.CachedTokens
	if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > cached {
		cached = usage.InputTokensDetails.CachedTokens
	}
	if usage.PromptCacheHitTokens > cached {
		cached = usage.PromptCacheHitTokens
	}
	if billingUsage, ok := usageFromBillingUsage(usage); ok {
		if billingUsage.PromptTokensDetails.CachedTokens > cached {
			cached = billingUsage.PromptTokensDetails.CachedTokens
		}
		if billingUsage.InputTokensDetails != nil && billingUsage.InputTokensDetails.CachedTokens > cached {
			cached = billingUsage.InputTokensDetails.CachedTokens
		}
		if billingUsage.PromptCacheHitTokens > cached {
			cached = billingUsage.PromptCacheHitTokens
		}
	}
	return cached
}

func promptCacheExpiryAccountingMode(usage *dto.Usage) string {
	billingUsage := usage
	if resolved, ok := usageFromBillingUsage(usage); ok {
		billingUsage = resolved
	}
	if billingUsage != nil && (billingUsage.UsageSemantic == "anthropic" || billingUsage.ClaudeCacheCreation5mTokens != 0 || billingUsage.ClaudeCacheCreation1hTokens != 0) {
		return relaycommon.PromptCacheExpiryAccountingSeparateFromInput
	}
	return relaycommon.PromptCacheExpiryAccountingIncludedInInput
}

// validPromptCacheExpiryUsage rejects usage the policy must not touch:
// negative or overflow-prone counts, cache reads exceeding the input total,
// and separate-from-input (anthropic) semantics, where zeroing cache reads
// would silently drop them from the bill instead of re-pricing them.
func validPromptCacheExpiryUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	counts := []int{
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.TotalTokens,
		usage.PromptCacheHitTokens,
		usage.InputTokens,
		usage.OutputTokens,
		usage.PromptTokensDetails.CachedTokens,
		usage.PromptTokensDetails.CachedCreationTokens,
		usage.PromptTokensDetails.CacheWriteTokens,
		usage.PromptTokensDetails.TextTokens,
		usage.PromptTokensDetails.AudioTokens,
		usage.PromptTokensDetails.ImageTokens,
		usage.CompletionTokenDetails.TextTokens,
		usage.CompletionTokenDetails.AudioTokens,
		usage.CompletionTokenDetails.ImageTokens,
		usage.CompletionTokenDetails.ReasoningTokens,
		usage.ClaudeCacheCreation5mTokens,
		usage.ClaudeCacheCreation1hTokens,
	}
	if usage.InputTokensDetails != nil {
		counts = append(counts,
			usage.InputTokensDetails.CachedTokens,
			usage.InputTokensDetails.CachedCreationTokens,
			usage.InputTokensDetails.CacheWriteTokens,
			usage.InputTokensDetails.TextTokens,
			usage.InputTokensDetails.AudioTokens,
			usage.InputTokensDetails.ImageTokens,
		)
	}
	billingUsage := usage
	if resolved, ok := usageFromBillingUsage(usage); ok {
		billingUsage = resolved
		counts = append(counts,
			resolved.PromptTokens,
			resolved.CompletionTokens,
			resolved.TotalTokens,
			resolved.PromptCacheHitTokens,
			resolved.InputTokens,
			resolved.OutputTokens,
			resolved.PromptTokensDetails.CachedTokens,
			resolved.PromptTokensDetails.CachedCreationTokens,
			resolved.PromptTokensDetails.CacheWriteTokens,
			resolved.PromptTokensDetails.TextTokens,
			resolved.PromptTokensDetails.AudioTokens,
			resolved.PromptTokensDetails.ImageTokens,
			resolved.CompletionTokenDetails.TextTokens,
			resolved.CompletionTokenDetails.AudioTokens,
			resolved.CompletionTokenDetails.ImageTokens,
			resolved.CompletionTokenDetails.ReasoningTokens,
			resolved.ClaudeCacheCreation5mTokens,
			resolved.ClaudeCacheCreation1hTokens,
		)
		if resolved.InputTokensDetails != nil {
			counts = append(counts,
				resolved.InputTokensDetails.CachedTokens,
				resolved.InputTokensDetails.CachedCreationTokens,
				resolved.InputTokensDetails.CacheWriteTokens,
				resolved.InputTokensDetails.TextTokens,
				resolved.InputTokensDetails.AudioTokens,
				resolved.InputTokensDetails.ImageTokens,
			)
		}
	}
	for _, count := range counts {
		if count < 0 || count > math.MaxInt32 {
			return false
		}
	}
	if usage.PromptTokens > 0 && usage.InputTokens > 0 && usage.PromptTokens != usage.InputTokens {
		return false
	}
	if usage.CompletionTokens > 0 && usage.OutputTokens > 0 && usage.CompletionTokens != usage.OutputTokens {
		return false
	}
	input := billingUsage.PromptTokens
	if input == 0 {
		input = billingUsage.InputTokens
	}
	output := billingUsage.CompletionTokens
	if output == 0 {
		output = billingUsage.OutputTokens
	}
	if input > math.MaxInt32-output {
		return false
	}
	if billingUsage.TotalTokens > 0 && billingUsage.TotalTokens != input+output {
		return false
	}
	projectedInput := promptCacheExpiryInputTotal(usage)
	if projectedInput > 0 && input > 0 && projectedInput != input {
		return false
	}
	cached := promptCacheExpiryCachedTokens(usage)
	if cached > input {
		return false
	}
	if promptCacheExpiryAccountingMode(usage) != relaycommon.PromptCacheExpiryAccountingIncludedInInput {
		return false
	}
	return true
}

// ApplyPromptCacheDiscountExpiry decides once whether this request owns the
// current 60-second cycle and, when it does, reclassifies the given usage:
// the input total stays unchanged and every cache-read alias is zeroed, so the
// full input is billed at the normal input price. The decision is sticky:
// claim success, claim failure, and Redis errors are all reused by later
// events (and retries) without another Redis call.
//
// claimEligible must be true only at protocol-approved claim points carrying
// real upstream usage: locally estimated usage, missing usage, and
// incomplete/failed terminals must pass false. With claimEligible=false the
// call still adjusts usage when an earlier claim already made this request
// the owner, keeping every later usage projection consistent.
func ApplyPromptCacheDiscountExpiry(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, claimEligible bool) bool {
	if info == nil || usage == nil {
		return false
	}
	st := resolvePromptCacheExpiryState(info)
	if st.AccountingMode == "" {
		st.AccountingMode = promptCacheExpiryAccountingMode(usage)
	}
	if st.Ineligible || st.SkipReason != "" {
		return false
	}
	if st.Decision == relaycommon.PromptCacheExpiryPending {
		if !claimEligible {
			return false
		}
		st.AccountingMode = promptCacheExpiryAccountingMode(usage)
		if !validPromptCacheExpiryUsage(usage) {
			st.SkipReason = promptCacheExpirySkipInvalidUsage
			logger.LogWarn(c, fmt.Sprintf("[prompt_cache_expiry] skip: invalid usage (input=%d cached=%d semantic=%q), upstream handling preserved", usage.PromptTokens, promptCacheExpiryCachedTokens(usage), usage.UsageSemantic))
			return false
		}
		originalUsage, err := clonePromptCacheExpiryUsage(usage)
		if err != nil {
			st.SkipReason = promptCacheExpirySkipInvalidUsage
			logger.LogWarn(c, fmt.Sprintf("[prompt_cache_expiry] skip: usage snapshot failed, upstream handling preserved: %s", err.Error()))
			return false
		}
		claim := promptCacheExpiryClaimFunc
		if claim == nil {
			// defensive: an active policy always wires the claim primitive at
			// startup; treat a missing one like a Redis failure (fail open)
			st.Decision = relaycommon.PromptCacheExpiryFailOpen
			logger.LogWarn(c, "[prompt_cache_expiry] claim primitive unavailable, fail-open (upstream discount preserved)")
			return false
		}
		owned, currentOwner, err := claim(promptCacheExpiryKeyPrefix+st.IdentityDigest, info.RequestId, PromptCacheExpiryCycleSeconds*time.Second)
		if err != nil {
			st.Decision = relaycommon.PromptCacheExpiryFailOpen
			logger.LogWarn(c, fmt.Sprintf("[prompt_cache_expiry] redis claim failed, fail-open (upstream discount preserved): %s", err.Error()))
			return false
		}
		st.OwnerRequestId = currentOwner
		if owned {
			st.Decision = relaycommon.PromptCacheExpiryOwner
			st.OriginalUsage = originalUsage
			st.RevokedCacheTokens = promptCacheExpiryCachedTokens(originalUsage)
		} else {
			st.Decision = relaycommon.PromptCacheExpiryInCycle
		}
	}
	if st.Decision != relaycommon.PromptCacheExpiryOwner {
		return false
	}
	if st.OriginalUsage == nil {
		originalUsage, err := clonePromptCacheExpiryUsage(usage)
		if err != nil {
			logger.LogWarn(c, fmt.Sprintf("[prompt_cache_expiry] owner snapshot unavailable, fail-open for this usage projection: %s", err.Error()))
			return false
		}
		st.OriginalUsage = originalUsage
		st.RevokedCacheTokens = promptCacheExpiryCachedTokens(usage)
	}
	usage.PromptTokensDetails.CachedTokens = 0
	if usage.InputTokensDetails != nil {
		usage.InputTokensDetails.CachedTokens = 0
	}
	usage.PromptCacheHitTokens = 0
	if usage.BillingUsage != nil {
		if openAIUsage := usage.BillingUsage.OpenAIUsage; openAIUsage != nil {
			openAIUsage.PromptTokensDetails.CachedTokens = 0
			if openAIUsage.InputTokensDetails != nil {
				openAIUsage.InputTokensDetails.CachedTokens = 0
			}
			openAIUsage.PromptCacheHitTokens = 0
		}
		if geminiUsage := usage.BillingUsage.GeminiUsageMetadata; geminiUsage != nil {
			geminiUsage.CachedContentTokenCount = 0
		}
	}
	st.Adjusted = true
	return true
}

func clonePromptCacheExpiryUsage(usage *dto.Usage) (*dto.Usage, error) {
	data, err := common.Marshal(usage)
	if err != nil {
		return nil, err
	}
	var clone dto.Usage
	if err := common.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

// ResponsesStatusCompleted reports whether a Responses payload is a
// protocol-successful completed response. Missing, malformed, incomplete, and
// failed statuses never open a billing cycle.
func ResponsesStatusCompleted(status json.RawMessage) bool {
	if common.GetJsonType(status) != "string" {
		return false
	}
	var s string
	if err := common.Unmarshal(status, &s); err != nil {
		return false
	}
	return s == "completed"
}

// ResponsesTerminalAllowsClaim limits claims to successful terminal events.
// response.done is emitted by some Responses-compatible upstreams.
func ResponsesTerminalAllowsClaim(eventType string, status json.RawMessage) bool {
	return (eventType == "response.completed" || eventType == "response.done") && ResponsesStatusCompleted(status)
}

// PatchResponsesUsageCachedTokens zeroes an existing cached-tokens field in a
// raw Responses JSON payload without disturbing any other byte. usagePath is
// "usage" for non-streaming bodies and "response.usage" for SSE events.
// Absent fields are never synthesized.
func PatchResponsesUsageCachedTokens(body []byte, usagePath string) []byte {
	patched := body
	for _, path := range []string{
		usagePath + ".input_tokens_details.cached_tokens",
		usagePath + ".prompt_tokens_details.cached_tokens",
		usagePath + ".prompt_cache_hit_tokens",
		usagePath + ".cached_tokens",
	} {
		if !gjson.GetBytes(patched, path).Exists() {
			continue
		}
		var err error
		patched, err = sjson.SetBytes(patched, path, 0)
		if err != nil {
			return body
		}
	}
	return patched
}

// PatchResponsesStreamUsageCachedTokens is the SSE-event string variant of
// PatchResponsesUsageCachedTokens.
func PatchResponsesStreamUsageCachedTokens(data string) string {
	return string(PatchResponsesUsageCachedTokens([]byte(data), "response.usage"))
}

// attachPromptCacheExpiryAudit nests the policy decision under the consume
// log's other.admin_info so it stays admin-only (non-admin log views strip
// admin_info). Raw identity values never appear; only a digest prefix does.
func attachPromptCacheExpiryAudit(relayInfo *relaycommon.RelayInfo, other map[string]interface{}) {
	if relayInfo == nil || other == nil || relayInfo.PromptCacheExpiry == nil {
		return
	}
	st := relayInfo.PromptCacheExpiry
	if st.Ineligible {
		return
	}
	audit := map[string]interface{}{
		"rule":        promptCacheExpiryRuleName,
		"ttl_seconds": PromptCacheExpiryCycleSeconds,
	}
	if st.AccountingMode != "" {
		audit["mode"] = st.AccountingMode
	}
	if st.IdentityDigest != "" {
		audit["identity_digest_prefix"] = st.IdentityDigest[:12]
		audit["identity_source"] = st.IdentitySource
	}
	switch {
	case st.SkipReason != "":
		audit["reason"] = "skip_" + st.SkipReason
	case st.Decision == relaycommon.PromptCacheExpiryOwner:
		audit["reason"] = "cycle_owner"
		audit["owner_request_id"] = st.OwnerRequestId
		audit["revoked_cache_tokens"] = st.RevokedCacheTokens
		if st.OriginalUsage != nil {
			audit["original_input_tokens"] = promptCacheExpiryInputTotal(st.OriginalUsage)
			audit["original_cached_tokens"] = promptCacheExpiryCachedTokens(st.OriginalUsage)
			audit["adjusted_input_tokens"] = promptCacheExpiryInputTotal(st.OriginalUsage)
			audit["adjusted_cached_tokens"] = 0
		}
	case st.Decision == relaycommon.PromptCacheExpiryInCycle:
		audit["reason"] = "in_cycle"
		audit["owner_request_id"] = st.OwnerRequestId
	case st.Decision == relaycommon.PromptCacheExpiryFailOpen:
		audit["reason"] = "redis_fail_open"
	default:
		// Eligible Codex request whose response never reached a claim point
		// (e.g. missing usage): record that the policy stayed pending.
		audit["reason"] = "no_claim_point"
	}
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	if !ok {
		adminInfo = make(map[string]interface{})
		other["admin_info"] = adminInfo
	}
	adminInfo["prompt_cache_discount_expiry"] = audit
}

func promptCacheExpiryInputTotal(usage *dto.Usage) int {
	if usage.PromptTokens != 0 {
		return usage.PromptTokens
	}
	return usage.InputTokens
}
