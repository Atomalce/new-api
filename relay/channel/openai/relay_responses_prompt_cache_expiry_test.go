package openai

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

func newResponsesExpiryTestContext(t *testing.T, body string, isStream bool) (*gin.Context, *httptest.ResponseRecorder, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-expiry-test")

	header := http.Header{"Content-Type": []string{"application/json"}}
	if isStream {
		header = http.Header{"Content-Type": []string{"text/event-stream"}}
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     header,
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.1-codex"},
		IsStream:    isStream,
		RelayFormat: types.RelayFormatOpenAIResponses,
		DisablePing: true,
	}
	return c, recorder, resp, info
}

const responsesExpiryNonStreamBody = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.1-codex","x_vendor":{"weird":true},"usage":{"input_tokens":10000,"output_tokens":500,"total_tokens":10500,"input_tokens_details":{"cached_tokens":8000,"vendor_extra":7}}}`

func TestOaiResponsesHandlerPromptCacheExpiryOwner(t *testing.T) {
	c, recorder, resp, info := newResponsesExpiryTestContext(t, responsesExpiryNonStreamBody, false)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// settlement sees full-price input: total input unchanged, cache read zeroed
	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 500, usage.CompletionTokens)
	assert.Equal(t, 10500, usage.TotalTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)

	// the client-visible body reports the same adjusted usage, with unknown
	// provider fields preserved byte-for-byte
	got := recorder.Body.String()
	assert.Contains(t, got, `"cached_tokens":0`)
	assert.NotContains(t, got, `"cached_tokens":8000`)
	assert.Contains(t, got, `"input_tokens":10000`)
	assert.Contains(t, got, `"vendor_extra":7`)
	assert.Contains(t, got, `"x_vendor":{"weird":true}`)

	// original upstream usage stays available for audit/telemetry
	st := info.PromptCacheExpiry
	require.NotNil(t, st.OriginalUsage)
	assert.Equal(t, 8000, st.OriginalUsage.PromptTokensDetails.CachedTokens)
	assert.True(t, st.Adjusted)
}

func TestOaiResponsesHandlerPromptCacheExpiryInCycle(t *testing.T) {
	c, recorder, resp, info := newResponsesExpiryTestContext(t, responsesExpiryNonStreamBody, false)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, responsesExpiryNonStreamBody, recorder.Body.String(),
		"non-owner responses must pass through byte-for-byte")
	assert.Nil(t, info.PromptCacheExpiry.OriginalUsage)
}

func TestOaiResponsesHandlerPromptCacheExpiryInactiveUnchanged(t *testing.T) {
	// the policy is inactive in tests (no Redis); a Codex-looking request must
	// behave exactly as before the feature existed
	c, recorder, resp, info := newResponsesExpiryTestContext(t, responsesExpiryNonStreamBody, false)
	info.RequestHeaders = map[string]string{"Originator": "Codex CLI", "Session_id": "sess-1"}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, responsesExpiryNonStreamBody, recorder.Body.String())
	require.NotNil(t, info.PromptCacheExpiry)
	assert.True(t, info.PromptCacheExpiry.Ineligible)
}

func responsesExpiryStreamBody() string {
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.1-codex"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10000,"output_tokens":500,"total_tokens":10500,"input_tokens_details":{"cached_tokens":8000}}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
}

func TestOaiResponsesStreamHandlerPromptCacheExpiryOwner(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	c, recorder, resp, info := newResponsesExpiryTestContext(t, responsesExpiryStreamBody(), true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 500, usage.CompletionTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)

	got := recorder.Body.String()
	assert.Contains(t, got, `"cached_tokens":0`)
	assert.NotContains(t, got, `"cached_tokens":8000`,
		"the terminal usage event must be patched before it is flushed")
	assert.Contains(t, got, `"input_tokens":10000`)
}

func TestOaiResponsesStreamHandlerPromptCacheExpiryInCycle(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	c, recorder, resp, info := newResponsesExpiryTestContext(t, responsesExpiryStreamBody(), true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
	assert.NotContains(t, recorder.Body.String(), `"cached_tokens":0`)
}

func TestOaiResponsesStreamHandlerIncompleteTerminalNeverClaims(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","usage":{"input_tokens":10000,"output_tokens":500,"total_tokens":10500,"input_tokens_details":{"cached_tokens":8000}}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesExpiryTestContext(t, body, true)
	// pending state on an eligible request: an incomplete terminal must not
	// reach the claim primitive at all
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)

	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"incomplete terminals must not attempt a claim")
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}

func TestOaiResponsesStreamHandlerIncompleteTerminalPatchesExistingOwner(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","usage":{"input_tokens":10000,"output_tokens":500,"total_tokens":10500,"input_tokens_details":{"cached_tokens":8000}}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesExpiryTestContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":0`)
}

func TestOaiResponsesStreamHandlerDoneTerminalOwner(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"type":"response.done","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10000,"output_tokens":500,"total_tokens":10500,"input_tokens_details":{"cached_tokens":8000,"cached_creation_tokens":123,"image_tokens":7}}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesExpiryTestContext(t, body, true)
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 123, usage.PromptTokensDetails.CachedCreationTokens)
	assert.Equal(t, 7, usage.PromptTokensDetails.ImageTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":0`)
}

func TestOaiChatToResponsesStreamHandlerPromptCacheExpiryOwner(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`,
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// the settled usage and the terminal response.completed event share the
	// same adjusted snapshot
	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)
	if usage.InputTokensDetails != nil {
		assert.Equal(t, 0, usage.InputTokensDetails.CachedTokens)
	}

	got := recorder.Body.String()
	assert.Contains(t, got, "response.completed")
	assert.NotContains(t, got, `"cached_tokens":8000`)
}

func TestOaiChatToResponsesStreamHandlerPromptCacheExpiryInCycle(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
		OwnerRequestId: "req-other",
	}

	usage, apiErr := OaiChatToResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}

func TestOaiChatToResponsesHandlerPromptCacheExpiryOwnerNonStream(t *testing.T) {
	body := `{"id":"chat_1","object":"chat.completion","model":"gpt-5.1-codex","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000}}}`
	c, recorder, resp, info := newResponsesChatTestContext(t, body, false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryOwner,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 10000, usage.PromptTokens)
	assert.Equal(t, 0, usage.PromptTokensDetails.CachedTokens)

	got := recorder.Body.String()
	assert.Contains(t, got, `"cached_tokens":0`)
	assert.NotContains(t, got, `"cached_tokens":8000`)
	assert.Contains(t, got, `"input_tokens":10000`)
}

func TestOaiChatToResponsesHandlerPromptCacheExpiryInCycleKeepsBillingDiscount(t *testing.T) {
	body := `{"id":"chat_1","object":"chat.completion","model":"gpt-5.1-codex","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000,"cached_creation_tokens":123,"image_tokens":7}}}`
	c, recorder, resp, info := newResponsesChatTestContext(t, body, false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryInCycle,
		IdentityDigest: "test-digest-000000000000",
		OwnerRequestId: "req-other",
	}

	usage, apiErr := OaiChatToResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 123, usage.PromptTokensDetails.CachedCreationTokens)
	assert.Equal(t, 7, usage.PromptTokensDetails.ImageTokens)
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}

func TestOaiChatToResponsesHandlerEstimatedUsageNeverClaims(t *testing.T) {
	// upstream reports no usage: the handler falls back to local estimation,
	// which must never reach the claim primitive
	body := `{"id":"chat_1","object":"chat.completion","model":"gpt-5.1-codex","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	c, _, resp, info := newResponsesChatTestContext(t, body, false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"locally estimated usage must not attempt a claim")
}

func TestOaiChatToResponsesHandlerEmptyChoicesNeverClaims(t *testing.T) {
	body := `{"id":"chat_1","object":"chat.completion","model":"gpt-5.1-codex","choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":0,"total_tokens":10000,"prompt_tokens_details":{"cached_tokens":8000}}}`
	c, _, resp, info := newResponsesChatTestContext(t, body, false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"an HTTP 200 response without protocol output must not open a cycle")
}

func TestOaiChatToResponsesHandlerMissingFinishReasonNeverClaims(t *testing.T) {
	body := `{"id":"chat_1","object":"chat.completion","model":"gpt-5.1-codex","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000}}}`
	c, _, resp, info := newResponsesChatTestContext(t, body, false)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"a response without a terminal finish reason must not open a cycle")
}

func TestOaiChatToResponsesStreamHandlerEstimatedUsageNeverClaims(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, _, resp, info := newResponsesChatTestContext(t, body, true)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Positive(t, usage.CompletionTokens)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"locally estimated stream usage must not attempt a claim")
}

func TestOaiChatToResponsesStreamHandlerAbnormalEOFWithUsageNeverClaims(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := strings.Join([]string{
		`data: {"id":"chat_1","object":"chat.completion.chunk","model":"gpt-5.1-codex","choices":[{"index":0,"delta":{"content":"hello"}}],"usage":{"prompt_tokens":10000,"completion_tokens":500,"total_tokens":10500,"prompt_tokens_details":{"cached_tokens":8000}}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
	info.RelayFormat = types.RelayFormatOpenAIResponses
	info.PromptCacheExpiry = &relaycommon.PromptCacheExpiryState{
		Decision:       relaycommon.PromptCacheExpiryPending,
		IdentityDigest: "test-digest-000000000000",
	}

	usage, apiErr := OaiChatToResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 8000, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, relaycommon.PromptCacheExpiryPending, info.PromptCacheExpiry.Decision,
		"an abnormal EOF without finish_reason must not open a cycle")
	assert.Contains(t, recorder.Body.String(), `"cached_tokens":8000`)
}
