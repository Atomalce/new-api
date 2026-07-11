package service

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCycleClaim struct {
	calls    int
	lastKey  string
	owned    bool
	owner    string
	err      error
	ownersBy map[string]string // simulated redis store: key -> owner
}

func (f *fakeCycleClaim) claim(key, owner string, ttl time.Duration) (bool, string, error) {
	f.calls++
	f.lastKey = key
	if f.err != nil {
		return false, "", f.err
	}
	if f.ownersBy != nil {
		if existing, ok := f.ownersBy[key]; ok {
			return existing == owner, existing, nil
		}
		f.ownersBy[key] = owner
		return true, owner, nil
	}
	return f.owned, f.owner, nil
}

func setupPromptCacheExpiry(t *testing.T, claim func(string, string, time.Duration) (bool, string, error)) {
	t.Helper()
	prevActive := promptCacheExpiryActive
	prevClaim := promptCacheExpiryClaimFunc
	promptCacheExpiryActive = true
	promptCacheExpiryClaimFunc = claim
	t.Cleanup(func() {
		promptCacheExpiryActive = prevActive
		promptCacheExpiryClaimFunc = prevClaim
	})
}

func codexRelayInfo(t *testing.T) *relaycommon.RelayInfo {
	t.Helper()
	return &relaycommon.RelayInfo{
		UserId:          42,
		RequestId:       "req-1",
		OriginModelName: "gpt-5.1-codex",
		RelayFormat:     types.RelayFormatOpenAIResponses,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-abc",
		},
		Request: &dto.OpenAIResponsesRequest{
			Model:          "gpt-5.1-codex",
			PromptCacheKey: json.RawMessage(`"pck-lineage-1"`),
		},
	}
}

func codexUsage() *dto.Usage {
	return &dto.Usage{
		PromptTokens:     10000,
		CompletionTokens: 500,
		TotalTokens:      10500,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:         8000,
			CachedCreationTokens: 123,
		},
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens:         8000,
			CachedCreationTokens: 123,
		},
		PromptCacheHitTokens: 8000,
	}
}

func testGinContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	return c
}

func usePromptCacheMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	previous := common.RDB
	common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		require.NoError(t, common.RDB.Close())
		common.RDB = previous
	})
	return server
}

func TestPromptCacheExpiryEligibilityNoOps(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(info *relaycommon.RelayInfo)
	}{
		{"non-responses format", func(info *relaycommon.RelayInfo) { info.RelayFormat = types.RelayFormatOpenAI }},
		{"compaction format", func(info *relaycommon.RelayInfo) { info.RelayFormat = types.RelayFormatOpenAIResponsesCompaction }},
		{"channel test", func(info *relaycommon.RelayInfo) { info.IsChannelTest = true }},
		{"prompt_cache_key alone is not codex", func(info *relaycommon.RelayInfo) {
			info.RequestHeaders = map[string]string{"User-Agent": "some-sdk"}
		}},
		{"no headers at all", func(info *relaycommon.RelayInfo) { info.RequestHeaders = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCycleClaim{owned: true}
			setupPromptCacheExpiry(t, fake.claim)
			info := codexRelayInfo(t)
			tt.mutate(info)
			usage := codexUsage()

			owner := ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true)

			require.False(t, owner)
			require.NotNil(t, info.PromptCacheExpiry)
			assert.True(t, info.PromptCacheExpiry.Ineligible)
			assert.Equal(t, 0, fake.calls, "ineligible requests must never touch redis")
			assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens, "usage must stay unchanged")
		})
	}
}

func TestPromptCacheExpiryInactivePolicyIsNoOp(t *testing.T) {
	fake := &fakeCycleClaim{owned: true}
	setupPromptCacheExpiry(t, fake.claim)
	promptCacheExpiryActive = false

	info := codexRelayInfo(t)
	usage := codexUsage()
	require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
	assert.True(t, info.PromptCacheExpiry.Ineligible)
	assert.Equal(t, 0, fake.calls)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
}

func TestPromptCacheExpiryIdentityResolution(t *testing.T) {
	digestOf := func(mutate func(info *relaycommon.RelayInfo)) string {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		mutate(info)
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true)
		require.NotNil(t, info.PromptCacheExpiry)
		return info.PromptCacheExpiry.IdentityDigest
	}

	base := digestOf(func(info *relaycommon.RelayInfo) {})
	require.NotEmpty(t, base)

	t.Run("body prompt_cache_key wins over session_id", func(t *testing.T) {
		conflicting := digestOf(func(info *relaycommon.RelayInfo) {
			info.RequestHeaders["Session_id"] = "another-session"
		})
		assert.Equal(t, base, conflicting)
	})

	t.Run("same logical value from either source shares one cycle", func(t *testing.T) {
		viaBody := base
		viaHeader := digestOf(func(info *relaycommon.RelayInfo) {
			info.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.1-codex"}
			info.RequestHeaders["Session_id"] = "pck-lineage-1"
		})
		assert.Equal(t, viaBody, viaHeader, "identity source must not split cycles")
	})

	t.Run("session_id fallback when body key absent", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		info.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.1-codex"}
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true)
		assert.Equal(t, "session_id", info.PromptCacheExpiry.IdentitySource)
		assert.NotEmpty(t, info.PromptCacheExpiry.IdentityDigest)
	})

	t.Run("non-string prompt_cache_key falls back to session_id", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		info.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.1-codex", PromptCacheKey: json.RawMessage(`123`)}
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true)
		assert.Equal(t, "session_id", info.PromptCacheExpiry.IdentitySource)
	})

	t.Run("different user or model opens a distinct cycle", func(t *testing.T) {
		otherUser := digestOf(func(info *relaycommon.RelayInfo) { info.UserId = 43 })
		otherModel := digestOf(func(info *relaycommon.RelayInfo) { info.OriginModelName = "gpt-5.2-codex" })
		assert.NotEqual(t, base, otherUser)
		assert.NotEqual(t, base, otherModel)
	})

	t.Run("routing and credential fields never split cycles", func(t *testing.T) {
		otherRouting := digestOf(func(info *relaycommon.RelayInfo) {
			info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: 99, ChannelType: 7, UpstreamModelName: "mapped-model"}
			info.TokenId = 123
			info.UsingGroup = "vip"
			info.RetryIndex = 2
		})
		assert.Equal(t, base, otherRouting,
			"channel, credential, token, group, and retry must not open extra cycles")
	})

	t.Run("missing identity skips with diagnostic", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		info.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.1-codex"}
		info.RequestHeaders = map[string]string{"Originator": "codex_cli_rs"}
		usage := codexUsage()

		require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
		assert.Equal(t, "missing_identity", info.PromptCacheExpiry.SkipReason)
		assert.Equal(t, 0, fake.calls)
		assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	})
}

func TestPromptCacheExpiryIdentityEncodingIsUnambiguous(t *testing.T) {
	setupPromptCacheExpiry(t, (&fakeCycleClaim{owned: true}).claim)
	digest := func(model, logicalId string) string {
		info := codexRelayInfo(t)
		info.OriginModelName = model
		raw, err := common.Marshal(logicalId)
		require.NoError(t, err)
		info.Request.(*dto.OpenAIResponsesRequest).PromptCacheKey = raw
		return resolvePromptCacheExpiryState(info).IdentityDigest
	}

	// These two pairs collide when fields are joined with newlines. Structured
	// encoding must keep model and logical-id boundaries distinct.
	first := digest("m", "x\ncodex_cache_lineage\ny")
	second := digest("m\ncodex_cache_lineage\nx", "y")
	assert.NotEqual(t, first, second)
}

func TestPromptCacheExpiryClaimStateMachine(t *testing.T) {
	t.Run("owner claims once and stays sticky", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true, owner: "req-1"}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		c := testGinContext(t)

		first := codexUsage()
		require.True(t, ApplyPromptCacheDiscountExpiry(c, info, first, true))
		assert.Equal(t, 1, fake.calls)
		assert.Equal(t, relaycommon.PromptCacheExpiryOwner, info.PromptCacheExpiry.Decision)
		assert.Equal(t, "req-1", info.PromptCacheExpiry.OwnerRequestId)

		// a later stream event reuses the decision without another redis call,
		// even with claimEligible=false
		second := codexUsage()
		require.True(t, ApplyPromptCacheDiscountExpiry(c, info, second, false))
		assert.Equal(t, 1, fake.calls)
		assert.Equal(t, 0, second.PromptTokensDetails.CachedTokens)
	})

	t.Run("in-cycle request preserves the discount", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: false, owner: "req-other"}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		usage := codexUsage()

		require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
		assert.Equal(t, relaycommon.PromptCacheExpiryInCycle, info.PromptCacheExpiry.Decision)
		assert.Equal(t, "req-other", info.PromptCacheExpiry.OwnerRequestId)
		assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
		assert.Nil(t, info.PromptCacheExpiry.OriginalUsage)
	})

	t.Run("redis error fails open and is request-sticky", func(t *testing.T) {
		fake := &fakeCycleClaim{err: fmt.Errorf("redis timeout")}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		c := testGinContext(t)
		usage := codexUsage()

		require.False(t, ApplyPromptCacheDiscountExpiry(c, info, usage, true))
		assert.Equal(t, relaycommon.PromptCacheExpiryFailOpen, info.PromptCacheExpiry.Decision)
		assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)

		// later events never retry the claim
		require.False(t, ApplyPromptCacheDiscountExpiry(c, info, codexUsage(), true))
		assert.Equal(t, 1, fake.calls)
	})

	t.Run("no claim without an eligible claim point", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)

		require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), false))
		assert.Equal(t, 0, fake.calls)
		assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision)
	})

	t.Run("replay with the same request id stays owner", func(t *testing.T) {
		fake := &fakeCycleClaim{ownersBy: map[string]string{}}
		setupPromptCacheExpiry(t, fake.claim)
		c := testGinContext(t)

		infoA := codexRelayInfo(t)
		require.True(t, ApplyPromptCacheDiscountExpiry(c, infoA, codexUsage(), true))

		// same request id, fresh state (e.g. replayed claim on retry)
		infoAReplay := codexRelayInfo(t)
		require.True(t, ApplyPromptCacheDiscountExpiry(c, infoAReplay, codexUsage(), true))

		// a different request inside the cycle is not the owner
		infoB := codexRelayInfo(t)
		infoB.RequestId = "req-2"
		usageB := codexUsage()
		require.False(t, ApplyPromptCacheDiscountExpiry(c, infoB, usageB, true))
		assert.Equal(t, "req-1", infoB.PromptCacheExpiry.OwnerRequestId)
		assert.Equal(t, 8000, usageB.PromptTokensDetails.CachedTokens)
	})
}

func TestPromptCacheExpiryUsageValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(u *dto.Usage)
	}{
		{"negative prompt tokens", func(u *dto.Usage) { u.PromptTokens = -1 }},
		{"negative completion tokens", func(u *dto.Usage) { u.CompletionTokens = -5 }},
		{"negative canonical cached tokens", func(u *dto.Usage) {
			u.PromptTokensDetails.CachedTokens = -1
			u.InputTokensDetails.CachedTokens = 0
			u.PromptCacheHitTokens = 0
		}},
		{"negative responses cached tokens", func(u *dto.Usage) {
			u.PromptTokensDetails.CachedTokens = 0
			u.InputTokensDetails.CachedTokens = -1
			u.PromptCacheHitTokens = 0
		}},
		{"cached exceeds input", func(u *dto.Usage) { u.PromptTokensDetails.CachedTokens = 10001 }},
		{"overflow-prone input", func(u *dto.Usage) { u.PromptTokens = math.MaxInt32 + 1 }},
		{"overflow-prone output", func(u *dto.Usage) { u.OutputTokens = math.MaxInt32 + 1 }},
		{"overflow-prone combined total", func(u *dto.Usage) {
			u.PromptTokens = math.MaxInt32
			u.CompletionTokens = 1
			u.TotalTokens = 0
		}},
		{"mismatched input aliases", func(u *dto.Usage) { u.InputTokens = 9999 }},
		{"mismatched total tokens", func(u *dto.Usage) { u.TotalTokens = 10499 }},
		{"anthropic semantic", func(u *dto.Usage) { u.UsageSemantic = "anthropic" }},
		{"claude cache creation split", func(u *dto.Usage) { u.ClaudeCacheCreation5mTokens = 10 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeCycleClaim{owned: true}
			setupPromptCacheExpiry(t, fake.claim)
			info := codexRelayInfo(t)
			usage := codexUsage()
			tt.mutate(usage)
			before := *usage

			require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
			assert.Equal(t, "invalid_usage", info.PromptCacheExpiry.SkipReason)
			assert.Equal(t, 0, fake.calls, "invalid usage must never claim")
			assert.Equal(t, before.PromptTokensDetails, usage.PromptTokensDetails)
			assert.Equal(t, before.PromptCacheHitTokens, usage.PromptCacheHitTokens)
		})
	}
}

func TestPromptCacheExpiryRejectsSeparateFromInputAccountingMode(t *testing.T) {
	fake := &fakeCycleClaim{owned: true}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	usage := codexUsage()
	usage.UsageSemantic = "anthropic"

	require.False(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
	assert.Equal(t, relaycommon.PromptCacheExpiryAccountingSeparateFromInput, info.PromptCacheExpiry.AccountingMode)
	assert.Equal(t, "invalid_usage", info.PromptCacheExpiry.SkipReason)
	assert.Equal(t, 0, fake.calls)
}

func TestPromptCacheExpiryReclassifyContract(t *testing.T) {
	fake := &fakeCycleClaim{owned: true, owner: "req-1"}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	usage := codexUsage()
	usage.Cost = map[string]any{"provider": map[string]any{"usd": 1.25}}

	require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))

	// PRD numeric contract: {input=10000, cached=8000, output=500} ->
	// owner {input=10000, cached=0, output=500}
	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 500, usage.CompletionTokens)
	assert.Equal(t, 10500, usage.TotalTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 0, usage.InputTokensDetails.CachedTokens)
	assert.Equal(t, 0, usage.PromptCacheHitTokens)
	// cache-creation dimensions stay untouched
	assert.Equal(t, 123, usage.PromptTokensDetails.CachedCreationTokens)
	assert.Equal(t, 123, usage.InputTokensDetails.CachedCreationTokens)

	st := info.PromptCacheExpiry
	require.NotNil(t, st.OriginalUsage)
	assert.Equal(t, 8000, st.RevokedCacheTokens)
	assert.Equal(t, 8000, st.OriginalUsage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 8000, st.OriginalUsage.InputTokensDetails.CachedTokens)

	// deep-copy isolation: mutating the adjusted usage (nested pointer
	// included) must not leak into the audit snapshot
	usage.InputTokensDetails.CachedTokens = 999
	usage.PromptTokensDetails.CachedTokens = 777
	usage.Cost.(map[string]any)["provider"].(map[string]any)["usd"] = 9.99
	assert.Equal(t, 8000, st.OriginalUsage.InputTokensDetails.CachedTokens)
	assert.Equal(t, 8000, st.OriginalUsage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 1.25, st.OriginalUsage.Cost.(map[string]any)["provider"].(map[string]any)["usd"])
}

func TestPromptCacheExpiryReclassifyNilDetailsPointer(t *testing.T) {
	fake := &fakeCycleClaim{owned: true}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	usage := codexUsage()
	usage.InputTokensDetails = nil

	require.NotPanics(t, func() {
		require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
	})
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)
	assert.Nil(t, usage.InputTokensDetails)
}

func TestPromptCacheExpiryReclassifiesCanonicalBillingUsage(t *testing.T) {
	t.Run("OpenAI billing usage", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true, owner: "req-1"}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		usage := codexUsage()
		usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)

		require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
		require.NotNil(t, usage.BillingUsage)
		require.NotNil(t, usage.BillingUsage.OpenAIUsage)
		assert.Equal(t, 0, usage.BillingUsage.OpenAIUsage.PromptTokensDetails.CachedTokens)
		assert.Equal(t, 0, effectiveBillingUsage(usage).PromptTokensDetails.CachedTokens)
		require.NotNil(t, info.PromptCacheExpiry.OriginalUsage)
		require.NotNil(t, info.PromptCacheExpiry.OriginalUsage.BillingUsage)
		assert.Equal(t, 8000, info.PromptCacheExpiry.OriginalUsage.BillingUsage.OpenAIUsage.PromptTokensDetails.CachedTokens)
	})

	t.Run("Gemini billing usage", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true, owner: "req-1"}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		usage := codexUsage()
		usage.BillingUsage = dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{
			PromptTokenCount:        10000,
			CandidatesTokenCount:    500,
			TotalTokenCount:         10500,
			CachedContentTokenCount: 8000,
		})

		require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
		require.NotNil(t, usage.BillingUsage)
		require.NotNil(t, usage.BillingUsage.GeminiUsageMetadata)
		assert.Equal(t, 0, usage.BillingUsage.GeminiUsageMetadata.CachedContentTokenCount)
		assert.Equal(t, 0, effectiveBillingUsage(usage).PromptTokensDetails.CachedTokens)
		require.NotNil(t, info.PromptCacheExpiry.OriginalUsage)
		require.NotNil(t, info.PromptCacheExpiry.OriginalUsage.BillingUsage)
		assert.Equal(t, 8000, info.PromptCacheExpiry.OriginalUsage.BillingUsage.GeminiUsageMetadata.CachedContentTokenCount)
	})
}

func TestPromptCacheExpiryNaturalMissOpensCycle(t *testing.T) {
	fake := &fakeCycleClaim{owned: true, owner: "req-1"}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	usage := &dto.Usage{PromptTokens: 10000, CompletionTokens: 500, TotalTokens: 10500}

	// a natural upstream miss (zero cache reads) still opens the cycle so the
	// next cache hit is not forced full price again
	require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))
	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, 0, info.PromptCacheExpiry.RevokedCacheTokens)
	assert.Equal(t, 10000, usage.PromptTokens)
}

func TestPromptCacheExpirySettlementContract(t *testing.T) {
	fake := &fakeCycleClaim{owned: true}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	usage := codexUsage()
	require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, usage, true))

	usedVars := map[string]bool{"cr": true, "p": true, "c": true, "len": true}

	// owner: tiered_expr receives p=10000, cr=0, len=10000
	params := BuildTieredTokenParams(usage, false, usedVars)
	assert.Equal(t, float64(10000), params.P)
	assert.Equal(t, float64(0), params.CR)
	assert.Equal(t, float64(10000), params.Len)
	assert.Equal(t, float64(500), params.C)

	// an in-cycle non-owner keeps the discount: p=2000, cr=8000, len=10000
	nonOwner := codexUsage()
	params = BuildTieredTokenParams(nonOwner, false, usedVars)
	assert.Equal(t, float64(2000), params.P)
	assert.Equal(t, float64(8000), params.CR)
	assert.Equal(t, float64(10000), params.Len)

	// Legacy ratio billing receives the same canonical owner/non-owner split.
	usage.PromptTokensDetails.CachedCreationTokens = 0
	if usage.InputTokensDetails != nil {
		usage.InputTokensDetails.CachedCreationTokens = 0
	}
	info.StartTime = time.Now()
	info.PriceData = types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
		CacheRatio:      0.1,
		GroupRatioInfo:  types.GroupRatioInfo{GroupRatio: 1},
	}
	summary := calculateTextQuotaSummary(testGinContext(t), info, usage)
	assert.Equal(t, 11000, summary.Quota, "owner cache reads must be charged as normal input")

	nonOwner.PromptTokensDetails.CachedCreationTokens = 0
	if nonOwner.InputTokensDetails != nil {
		nonOwner.InputTokensDetails.CachedCreationTokens = 0
	}
	nonOwnerInfo := codexRelayInfo(t)
	nonOwnerInfo.StartTime = time.Now()
	nonOwnerInfo.PriceData = info.PriceData
	nonOwnerSummary := calculateTextQuotaSummary(testGinContext(t), nonOwnerInfo, nonOwner)
	assert.Equal(t, 3800, nonOwnerSummary.Quota, "in-cycle cache reads must keep the configured discount")

	// Fixed per-call pricing is usage-independent and therefore invariant.
	fixedInfo := codexRelayInfo(t)
	fixedInfo.StartTime = time.Now()
	fixedInfo.PriceData = types.PriceData{
		UsePrice:       true,
		ModelPrice:     0.02,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	originalFixed := codexUsage()
	adjustedFixed := *originalFixed
	adjustedFixed.PromptTokensDetails.CachedTokens = 0
	adjustedFixed.InputTokensDetails = &dto.InputTokenDetails{}
	assert.Equal(t,
		calculateTextQuotaSummary(testGinContext(t), fixedInfo, originalFixed).Quota,
		calculateTextQuotaSummary(testGinContext(t), fixedInfo, &adjustedFixed).Quota,
	)
}

func TestPatchResponsesUsageCachedTokens(t *testing.T) {
	t.Run("zeroes existing field and preserves unknown bytes", func(t *testing.T) {
		body := []byte(`{"id":"resp_1","usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":8000,"vendor_extra":7},"output_tokens":500},"x_vendor":{"weird":true}}`)
		patched := PatchResponsesUsageCachedTokens(body, "usage")
		assert.Contains(t, string(patched), `"cached_tokens":0`)
		assert.Contains(t, string(patched), `"vendor_extra":7`)
		assert.Contains(t, string(patched), `"x_vendor":{"weird":true}`)
		assert.Contains(t, string(patched), `"input_tokens":10000`)
		assert.Contains(t, string(patched), `"output_tokens":500`)
	})

	t.Run("never synthesizes an absent field", func(t *testing.T) {
		body := []byte(`{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":5}}`)
		patched := PatchResponsesUsageCachedTokens(body, "usage")
		assert.Equal(t, string(body), string(patched))
	})

	t.Run("stream event variant patches response.usage", func(t *testing.T) {
		data := `{"type":"response.completed","response":{"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":8000},"output_tokens":500}}}`
		patched := PatchResponsesStreamUsageCachedTokens(data)
		assert.Contains(t, patched, `"cached_tokens":0`)
		assert.NotContains(t, patched, `"cached_tokens":8000`)
	})
}

func TestResponsesClaimEligibility(t *testing.T) {
	assert.False(t, ResponsesStatusCompleted(nil))
	assert.True(t, ResponsesStatusCompleted(json.RawMessage(`"completed"`)))
	assert.False(t, ResponsesStatusCompleted(json.RawMessage(`null`)))
	assert.False(t, ResponsesStatusCompleted(json.RawMessage(`"incomplete"`)))
	assert.False(t, ResponsesStatusCompleted(json.RawMessage(`"failed"`)))
	assert.True(t, ResponsesTerminalAllowsClaim("response.completed", json.RawMessage(`"completed"`)))
	assert.True(t, ResponsesTerminalAllowsClaim("response.done", json.RawMessage(`"completed"`)))
	assert.False(t, ResponsesTerminalAllowsClaim("response.incomplete", json.RawMessage(`"completed"`)))
	assert.False(t, ResponsesTerminalAllowsClaim("response.done", json.RawMessage(`"failed"`)))
}

func TestPromptCacheExpiryAudit(t *testing.T) {
	t.Run("owner audit is admin-only and never leaks raw identity", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true, owner: "req-1"}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true))

		other := map[string]interface{}{}
		attachPromptCacheExpiryAudit(info, other)

		adminInfo, ok := other["admin_info"].(map[string]interface{})
		require.True(t, ok)
		audit, ok := adminInfo["prompt_cache_discount_expiry"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "cycle_owner", audit["reason"])
		assert.Equal(t, "codex_responses_60s", audit["rule"])
		assert.Equal(t, relaycommon.PromptCacheExpiryAccountingIncludedInInput, audit["mode"])
		assert.Equal(t, 60, audit["ttl_seconds"])
		assert.Equal(t, "req-1", audit["owner_request_id"])
		assert.Equal(t, 8000, audit["revoked_cache_tokens"])
		assert.Equal(t, 10000, audit["original_input_tokens"])
		assert.Equal(t, 8000, audit["original_cached_tokens"])
		assert.Equal(t, 10000, audit["adjusted_input_tokens"])
		assert.Equal(t, 0, audit["adjusted_cached_tokens"])
		assert.Equal(t, info.PromptCacheExpiry.IdentityDigest[:12], audit["identity_digest_prefix"])

		serialized, err := common.Marshal(other)
		require.NoError(t, err)
		assert.NotContains(t, string(serialized), "pck-lineage-1")
		assert.NotContains(t, string(serialized), "sess-abc")
		assert.NotContains(t, string(serialized), info.PromptCacheExpiry.IdentityDigest,
			"full digest must not be logged, only the prefix")
	})

	t.Run("ineligible requests emit no audit", func(t *testing.T) {
		fake := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		info.RequestHeaders = nil
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true)

		other := map[string]interface{}{}
		attachPromptCacheExpiryAudit(info, other)
		assert.Empty(t, other)
	})

	t.Run("skip and fail-open reasons are recorded", func(t *testing.T) {
		fake := &fakeCycleClaim{err: fmt.Errorf("redis down")}
		setupPromptCacheExpiry(t, fake.claim)
		info := codexRelayInfo(t)
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true)

		other := map[string]interface{}{}
		attachPromptCacheExpiryAudit(info, other)
		audit := other["admin_info"].(map[string]interface{})["prompt_cache_discount_expiry"].(map[string]interface{})
		assert.Equal(t, "redis_fail_open", audit["reason"])

		fake2 := &fakeCycleClaim{owned: true}
		setupPromptCacheExpiry(t, fake2.claim)
		info2 := codexRelayInfo(t)
		info2.Request = &dto.OpenAIResponsesRequest{Model: "gpt-5.1-codex"}
		info2.RequestHeaders = map[string]string{"Originator": "codex_cli_rs"}
		ApplyPromptCacheDiscountExpiry(testGinContext(t), info2, codexUsage(), true)

		other2 := map[string]interface{}{}
		attachPromptCacheExpiryAudit(info2, other2)
		audit2 := other2["admin_info"].(map[string]interface{})["prompt_cache_discount_expiry"].(map[string]interface{})
		assert.Equal(t, "skip_missing_identity", audit2["reason"])
	})
}

func TestInitPromptCacheDiscountExpiryWithoutRedisIsCompatibilityNoOp(t *testing.T) {
	prevEnabled := common.RedisEnabled
	prevActive := promptCacheExpiryActive
	t.Cleanup(func() {
		common.RedisEnabled = prevEnabled
		promptCacheExpiryActive = prevActive
	})
	common.RedisEnabled = false
	promptCacheExpiryActive = true

	InitPromptCacheDiscountExpiry()

	assert.False(t, promptCacheExpiryActive,
		"without Redis the process must start with the policy inactive")
}

func TestValidatePromptCacheExpiryStartupRequiresExplicitSecret(t *testing.T) {
	t.Setenv("CRYPTO_SECRET", "")
	t.Setenv("SESSION_SECRET", "")

	err := validatePromptCacheExpiryStartup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CRYPTO_SECRET")
}

func TestValidatePromptCacheExpiryStartupSentinel(t *testing.T) {
	server := usePromptCacheMiniRedis(t)
	previousSecret := common.CryptoSecret
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	t.Setenv("SESSION_SECRET", "")
	t.Setenv("CRYPTO_SECRET", "shared-secret-a")
	common.CryptoSecret = "shared-secret-a"
	require.NoError(t, validatePromptCacheExpiryStartup())
	assert.Equal(t, time.Duration(0), server.TTL(promptCacheExpirySentinelKey), "the cross-node secret sentinel must not expire")
	require.NoError(t, validatePromptCacheExpiryStartup(), "a second node with the same secret must pass")

	t.Setenv("CRYPTO_SECRET", "shared-secret-b")
	common.CryptoSecret = "shared-secret-b"
	err := validatePromptCacheExpiryStartup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fingerprint mismatch")
}

func TestValidatePromptCacheExpiryStartupRejectsDefaultSecret(t *testing.T) {
	previousSecret := common.CryptoSecret
	t.Cleanup(func() { common.CryptoSecret = previousSecret })
	t.Setenv("SESSION_SECRET", "random_string")
	t.Setenv("CRYPTO_SECRET", "")
	common.CryptoSecret = "random_string"

	err := validatePromptCacheExpiryStartup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-default shared secret")
}

func TestValidatePromptCacheExpiryStartupRejectsUnhealthyRedis(t *testing.T) {
	previousClient := common.RDB
	previousSecret := common.CryptoSecret
	common.RDB = redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 20 * time.Millisecond,
		ReadTimeout: 20 * time.Millisecond,
	})
	t.Cleanup(func() {
		require.NoError(t, common.RDB.Close())
		common.RDB = previousClient
		common.CryptoSecret = previousSecret
	})
	t.Setenv("SESSION_SECRET", "")
	t.Setenv("CRYPTO_SECRET", "shared-secret")
	common.CryptoSecret = "shared-secret"

	err := validatePromptCacheExpiryStartup()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "health check failed")
}

func TestPromptCacheExpiryClaimKeyScopedToDigest(t *testing.T) {
	fake := &fakeCycleClaim{owned: true}
	setupPromptCacheExpiry(t, fake.claim)
	info := codexRelayInfo(t)
	require.True(t, ApplyPromptCacheDiscountExpiry(testGinContext(t), info, codexUsage(), true))
	require.True(t, strings.HasPrefix(fake.lastKey, "pce:v1:c:"))
	assert.Equal(t, "pce:v1:c:"+info.PromptCacheExpiry.IdentityDigest, fake.lastKey)
}
