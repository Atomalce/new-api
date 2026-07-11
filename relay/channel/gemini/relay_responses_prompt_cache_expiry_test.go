package gemini

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGeminiResponsesExpiryContext(t *testing.T, body string, isStream bool) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "gemini-expiry-test")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gemini-2.5-pro"},
		IsStream:    isStream,
		RelayFormat: types.RelayFormatOpenAIResponses,
		DisablePing: true,
	}
	return c, recorder, resp, info
}

const geminiResponsesExpiryBody = `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10000,"candidatesTokenCount":500,"totalTokenCount":10500,"cachedContentTokenCount":8000}}`

func TestGeminiResponsesHandlerPromptCacheExpiryOwner(t *testing.T) {
	c, recorder, resp, info := newGeminiResponsesExpiryContext(t, geminiResponsesExpiryBody, false)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := GeminiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// settlement usage: full input at normal price, cache read zeroed
	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 500, usage.CompletionTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)

	// the projected Responses payload reports the same adjusted usage
	got := recorder.Body.String()
	assert.Contains(t, got, `"input_tokens":10000`)
	assert.NotContains(t, got, `"cached_tokens":8000`)

	st := info.PromptCacheExpiry
	require.NotNil(t, st.OriginalUsage)
	assert.Equal(t, 8000, st.OriginalUsage.PromptTokensDetails.CachedTokens)
}

func TestGeminiResponsesHandlerPromptCacheExpiryInCycle(t *testing.T) {
	c, recorder, resp, info := newGeminiResponsesExpiryContext(t, geminiResponsesExpiryBody, false)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := GeminiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}

func TestGeminiResponsesStreamHandlerPromptCacheExpiryOwner(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: ` + geminiResponsesExpiryBody,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newGeminiResponsesExpiryContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := GeminiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)

	got := recorder.Body.String()
	assert.Contains(t, got, "response.completed")
	assert.NotContains(t, got, `"cached_tokens":8000`,
		"the synthesized terminal event must project the adjusted usage")
}

func TestGeminiResponsesStreamHandlerPromptCacheExpiryInCycle(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: ` + geminiResponsesExpiryBody,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newGeminiResponsesExpiryContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
		OwnerRequestId: "req-other",
	}

	usage, apiErr := GeminiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}

func TestGeminiResponsesStreamHandlerEstimatedUsageNeverClaims(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	// no usageMetadata at all: the stream handler falls back to local
	// estimation, which must never reach the claim primitive
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, _, resp, info := newGeminiResponsesExpiryContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	_, apiErr := GeminiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"estimated usage must not attempt a claim")
}

func TestGeminiResponsesStreamHandlerAbnormalEOFWithUsageNeverClaims(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":10000,"candidatesTokenCount":500,"totalTokenCount":10500,"cachedContentTokenCount":8000}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newGeminiResponsesExpiryContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := GeminiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"an abnormal EOF without a terminal Gemini finish reason must not open a cycle")
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}
