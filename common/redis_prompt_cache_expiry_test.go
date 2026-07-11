package common

import (
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func useMiniRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	previous := RDB
	RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		require.NoError(t, RDB.Close())
		RDB = previous
	})
	return server
}

func TestRedisClaimCycleOwnerContract(t *testing.T) {
	server := useMiniRedis(t)

	owned, owner, err := RedisClaimCycleOwner("cycle", "request-a", 3*time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	assert.Equal(t, "request-a", owner)
	firstTTL := server.TTL("cycle")

	server.FastForward(time.Minute)
	owned, owner, err = RedisClaimCycleOwner("cycle", "request-a", 3*time.Minute)
	require.NoError(t, err)
	require.True(t, owned)
	assert.Equal(t, "request-a", owner)
	assert.Equal(t, firstTTL-time.Minute, server.TTL("cycle"), "owner replay must not refresh the cycle TTL")

	owned, owner, err = RedisClaimCycleOwner("cycle", "request-b", 3*time.Minute)
	require.NoError(t, err)
	assert.False(t, owned)
	assert.Equal(t, "request-a", owner)
}

func TestRedisClaimCycleOwnerRejectsInvalidTTL(t *testing.T) {
	useMiniRedis(t)

	_, _, err := RedisClaimCycleOwner("cycle", "request-a", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one second")
}

func TestRedisClaimCycleOwnerConcurrentBoundary(t *testing.T) {
	useMiniRedis(t)

	const requests = 16
	var wg sync.WaitGroup
	winners := make(chan string, requests)
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		owner := string(rune('a' + i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			owned, _, err := RedisClaimCycleOwner("concurrent-cycle", owner, 3*time.Minute)
			if err != nil {
				errs <- err
				return
			}
			if owned {
				winners <- owner
			}
		}()
	}
	wg.Wait()
	close(winners)
	close(errs)

	assert.Empty(t, errs)
	assert.Len(t, winners, 1)
}

func TestRedisEnsurePersistentValue(t *testing.T) {
	server := useMiniRedis(t)

	matched, existing, err := RedisEnsurePersistentValue("sentinel", "fingerprint-a")
	require.NoError(t, err)
	require.True(t, matched)
	assert.Equal(t, "fingerprint-a", existing)
	assert.Equal(t, time.Duration(0), server.TTL("sentinel"))

	server.SetTTL("sentinel", time.Hour)
	matched, existing, err = RedisEnsurePersistentValue("sentinel", "fingerprint-a")
	require.NoError(t, err)
	require.True(t, matched)
	assert.Equal(t, "fingerprint-a", existing)
	assert.Equal(t, time.Duration(0), server.TTL("sentinel"), "matching legacy sentinels must have their TTL removed")

	matched, existing, err = RedisEnsurePersistentValue("sentinel", "fingerprint-b")
	require.NoError(t, err)
	assert.False(t, matched)
	assert.Equal(t, "fingerprint-a", existing)
}
