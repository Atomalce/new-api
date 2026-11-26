package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// useTestChannelBreaker installs a fresh breaker on a manual clock with the
// default thresholds (3 failures, 30s cooldown doubling up to 300s).
func useTestChannelBreaker(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	previousBreaker, previousSetting := channelBreaker, *operation_setting.GetChannelCircuitSetting()
	channelBreaker = newChannelCircuitBreaker(func() time.Time { return now })
	*operation_setting.GetChannelCircuitSetting() = operation_setting.ChannelCircuitSetting{
		Enabled: true, FailureThreshold: 3, CooldownSeconds: 30, MaxCooldownSeconds: 300,
	}
	t.Cleanup(func() {
		channelBreaker = previousBreaker
		*operation_setting.GetChannelCircuitSetting() = previousSetting
	})
	return &now
}

func upstreamStatusError(status int) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, status)
}

func TestChannelCircuitLifecycle(t *testing.T) {
	now := useTestChannelBreaker(t)
	const id = 7

	channelBreaker.recordFailure(id)
	channelBreaker.recordFailure(id)
	assert.True(t, channelBreaker.allows(id), "two failures stay under the threshold")

	*now = now.Add(2 * time.Minute)
	channelBreaker.recordFailure(id)
	assert.True(t, channelBreaker.allows(id), "failures outside the window start a new count")

	channelBreaker.recordFailure(id)
	channelBreaker.recordFailure(id)
	assert.False(t, channelBreaker.allows(id), "the third consecutive failure opens the circuit")
	assert.Equal(t, []int{id}, channelBreaker.unavailable())

	*now = now.Add(10 * time.Second)
	channelBreaker.recordFailure(id)
	*now = now.Add(21 * time.Second)
	assert.True(t, channelBreaker.allows(id), "failures of in-flight requests do not extend the cooldown")

	channelBreaker.claim(id)
	assert.False(t, channelBreaker.allows(id), "only the claiming request probes a half-open channel")
	*now = now.Add(channelCircuitProbeLease)
	assert.True(t, channelBreaker.allows(id), "an unreported probe frees the slot after its lease")

	channelBreaker.claim(id)
	channelBreaker.recordFailure(id)
	*now = now.Add(59 * time.Second)
	assert.False(t, channelBreaker.allows(id), "a failed probe doubles the cooldown")
	*now = now.Add(time.Second)
	assert.True(t, channelBreaker.allows(id))

	for range 5 {
		channelBreaker.claim(id)
		channelBreaker.recordFailure(id)
		*now = now.Add(300 * time.Second)
	}
	assert.True(t, channelBreaker.allows(id), "the cooldown is capped at the maximum")

	channelBreaker.recordSuccess(id)
	channelBreaker.recordFailure(id)
	assert.True(t, channelBreaker.allows(id), "a success closes the circuit and resets the count")

	operation_setting.GetChannelCircuitSetting().Enabled = false
	channelBreaker.recordFailure(id)
	channelBreaker.recordFailure(id)
	assert.True(t, channelBreaker.allows(id))
	assert.Empty(t, channelBreaker.unavailable())
}

func TestRecordChannelAttemptFailureCountsOnlyChannelFaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name      string
		err       *types.NewAPIError
		ctx       context.Context
		wantCount bool
	}{
		{"upstream 500", upstreamStatusError(http.StatusInternalServerError), context.Background(), true},
		{"upstream 429", upstreamStatusError(http.StatusTooManyRequests), context.Background(), true},
		{"upstream 401", upstreamStatusError(http.StatusUnauthorized), context.Background(), true},
		{"transport failure", types.NewError(errors.New("dial tcp: timeout"), types.ErrorCodeDoRequestFailed), context.Background(), true},
		{"upstream 400", upstreamStatusError(http.StatusBadRequest), context.Background(), false},
		{"upstream 413", upstreamStatusError(http.StatusRequestEntityTooLarge), context.Background(), false},
		{"local error", types.NewErrorWithStatusCode(errors.New("bad body"), types.ErrorCodeInvalidRequest, http.StatusInternalServerError), context.Background(), false},
		{"client left", upstreamStatusError(http.StatusBadGateway), cancelled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTestChannelBreaker(t)
			const id = 11
			for range 3 {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(tc.ctx)
				RecordChannelAttemptFailure(c, id, tc.err)
				assert.Equal(t, []int{id}, requestFailedChannelIDs(c), "retries of the request skip the channel either way")
			}
			assert.Equal(t, tc.wantCount, !channelBreaker.allows(id))
		})
	}
}

func createCircuitTestChannel(t *testing.T, db *gorm.DB, id int, priority int64, modelName string) {
	t.Helper()
	weight := uint(100)
	require.NoError(t, db.Create(&model.Channel{
		Id: id, Type: constant.ChannelTypeOpenAI, Key: fmt.Sprintf("key-%d", id), Status: common.ChannelStatusEnabled,
		Name: fmt.Sprintf("channel-%d", id), Weight: &weight, Models: modelName, Group: "default", Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: modelName, ChannelId: id, Enabled: true, Priority: &priority, Weight: weight,
	}).Error)
}

// The database path (memory cache off) of the exclusion filter is covered in
// the model package, which can initialize the dialect's column names.
func TestCacheGetRandomSatisfiedChannelPrefersUntriedHealthyChannels(t *testing.T) {
	const modelName = "circuit-retry-model"
	db := setupChannelSelectAutoGroupsTest(t)
	createCircuitTestChannel(t, db, 3101, 10, modelName)
	createCircuitTestChannel(t, db, 3102, 5, modelName)
	createCircuitTestChannel(t, db, 3103, 0, modelName)
	model.InitChannelCache()
	useTestChannelBreaker(t)

	pick := func(c *gin.Context, retry int) int {
		t.Helper()
		channel, _, err := CacheGetRandomSatisfiedChannel(&RetryParam{Ctx: c, TokenGroup: "default", ModelName: modelName, Retry: &retry})
		require.NoError(t, err)
		require.NotNil(t, channel)
		return channel.Id
	}
	newRequest := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c
	}
	fail := func(c *gin.Context, id int) {
		RecordChannelAttemptFailure(c, id, upstreamStatusError(http.StatusBadGateway))
	}

	c := newRequest()
	assert.Equal(t, 3101, pick(c, 0))
	fail(c, 3101)
	assert.Equal(t, 3102, pick(c, 1), "the next tier is tried instead of being skipped")
	fail(c, 3102)
	assert.Equal(t, 3103, pick(c, 2))
	fail(c, 3103)
	assert.Equal(t, 3103, pick(c, 3), "with every channel tried, the priority retry pick is used again")

	for range 2 {
		fail(newRequest(), 3101)
	}
	require.False(t, channelBreaker.allows(3101))
	assert.Equal(t, 3102, pick(newRequest(), 0), "a new request passes over the open circuit")

	c = newRequest()
	fail(c, 3102)
	fail(newRequest(), 3102)
	fail(newRequest(), 3103)
	fail(newRequest(), 3103)
	require.False(t, channelBreaker.allows(3102))
	require.False(t, channelBreaker.allows(3103))
	assert.Equal(t, 3101, pick(c, 1), "an untried channel with an open circuit beats a channel that failed this request")

	assert.Equal(t, 3101, pick(newRequest(), 0), "a fully cooled-down pool is still used by priority")
}
