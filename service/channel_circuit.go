package service

import (
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

const (
	// Failures further apart than this do not add up to a trip, so a channel
	// with sparse traffic is not cooled down by unrelated errors hours apart.
	channelCircuitFailureWindow = time.Minute
	// A recovery probe that never reports back (a request rejected before the
	// relay, a long stream) frees the probe slot after this long.
	channelCircuitProbeLease = time.Minute

	ginKeyFailedChannelIDs = "failed_channel_ids"
)

// channelCircuit is one channel's breaker state. openUntil is zero while the
// circuit is closed. After it passes the circuit is half-open: one request may
// probe the channel (probeUntil is its lease), a success closes the circuit
// and a failure opens it again with twice the cooldown.
type channelCircuit struct {
	failures    int
	lastFailure time.Time
	openUntil   time.Time
	cooldown    time.Duration
	probeUntil  time.Time
}

// channelCircuitBreaker keeps the circuits of this process in memory; every
// instance of a multi-node deployment judges channels on its own traffic.
// Entries exist only for channels that failed since their last success.
type channelCircuitBreaker struct {
	mu       sync.Mutex
	circuits map[int]*channelCircuit
	now      func() time.Time
}

var channelBreaker = newChannelCircuitBreaker(time.Now)

func newChannelCircuitBreaker(now func() time.Time) *channelCircuitBreaker {
	return &channelCircuitBreaker{circuits: make(map[int]*channelCircuit), now: now}
}

// blocks reports a circuit that is cooling down or whose recovery probe is in
// flight. Caller holds b.mu.
func (circuit *channelCircuit) blocks(now time.Time) bool {
	return !circuit.openUntil.IsZero() && (now.Before(circuit.openUntil) || now.Before(circuit.probeUntil))
}

// unavailable lists the channels selection should pass over.
func (b *channelCircuitBreaker) unavailable() []int {
	if !operation_setting.GetChannelCircuitSetting().Enabled {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var ids []int
	for id, circuit := range b.circuits {
		if circuit.blocks(now) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *channelCircuitBreaker) allows(channelID int) bool {
	if !operation_setting.GetChannelCircuitSetting().Enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	circuit := b.circuits[channelID]
	return circuit == nil || !circuit.blocks(b.now())
}

// claim gives the recovery probe of a half-open channel to the request that
// selected it, so other requests keep passing the channel over until the
// probe reports back.
func (b *channelCircuitBreaker) claim(channelID int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	circuit := b.circuits[channelID]
	now := b.now()
	if circuit == nil || circuit.openUntil.IsZero() || circuit.blocks(now) {
		return
	}
	circuit.probeUntil = now.Add(channelCircuitProbeLease)
}

func (b *channelCircuitBreaker) recordSuccess(channelID int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	circuit := b.circuits[channelID]
	if circuit == nil {
		return
	}
	if !circuit.openUntil.IsZero() {
		common.SysLog(common.LogText("channel circuit closed: channel #%d recovered", channelID))
	}
	delete(b.circuits, channelID)
}

func (b *channelCircuitBreaker) recordFailure(channelID int) {
	setting := operation_setting.GetChannelCircuitSetting()
	if !setting.Enabled {
		return
	}
	baseCooldown := time.Duration(max(setting.CooldownSeconds, 1)) * time.Second
	maxCooldown := max(time.Duration(setting.MaxCooldownSeconds)*time.Second, baseCooldown)

	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	circuit := b.circuits[channelID]
	if circuit == nil {
		circuit = &channelCircuit{}
		b.circuits[channelID] = circuit
	}
	if circuit.openUntil.IsZero() {
		if now.Sub(circuit.lastFailure) > channelCircuitFailureWindow {
			circuit.failures = 0
		}
		circuit.failures++
		circuit.lastFailure = now
		if circuit.failures < max(setting.FailureThreshold, 1) {
			return
		}
		circuit.cooldown = baseCooldown
	} else {
		// Requests already in flight when the circuit opened fail as well;
		// only a failure after the cooldown, a failed probe, opens it again.
		if now.Before(circuit.openUntil) {
			return
		}
		circuit.cooldown = min(circuit.cooldown*2, maxCooldown)
	}
	circuit.openUntil = now.Add(circuit.cooldown)
	circuit.probeUntil = time.Time{}
	common.SysLog(common.LogText("channel circuit opened: channel #%d is passed over for %s after %d consecutive failures", channelID, circuit.cooldown, circuit.failures))
}

// RecordChannelAttemptFailure notes that this request failed on channelID,
// so its retries try other channels first, and counts the failure against the
// channel's circuit when the channel is to blame: no answer from the upstream,
// a channel key or configuration error, or an upstream 5xx, 429, 408, 401 or
// 403. Errors new-api raised itself, errors caused by the request (such as
// 400 or 413) and failures after the client left do not count.
func RecordChannelAttemptFailure(c *gin.Context, channelID int, err *types.NewAPIError) {
	if c == nil || channelID <= 0 || err == nil {
		return
	}
	failed := requestFailedChannelIDs(c)
	if !slices.Contains(failed, channelID) {
		c.Set(ginKeyFailedChannelIDs, append(failed, channelID))
	}
	if c.Request != nil && c.Request.Context().Err() != nil {
		return
	}
	switch RelayErrorSource(err) {
	case "local":
		return
	case "upstream":
		switch code := err.StatusCode; {
		case code < 100, code >= http.StatusInternalServerError:
		case code == http.StatusTooManyRequests, code == http.StatusRequestTimeout,
			code == http.StatusUnauthorized, code == http.StatusForbidden:
		default:
			return
		}
	}
	channelBreaker.recordFailure(channelID)
}

// RecordChannelAttemptSuccess closes the channel's circuit once the upstream
// answered a request.
func RecordChannelAttemptSuccess(channelID int) {
	channelBreaker.recordSuccess(channelID)
}

func requestFailedChannelIDs(c *gin.Context) []int {
	value, _ := c.Get(ginKeyFailedChannelIDs)
	ids, _ := value.([]int)
	return ids
}
