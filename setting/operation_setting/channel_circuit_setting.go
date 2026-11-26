package operation_setting

import "github.com/QuantumNous/new-api/setting/config"

// ChannelCircuitSetting configures the in-memory channel circuit breaker.
// A channel that fails FailureThreshold requests in a row is passed over for
// CooldownSeconds; each failed recovery probe doubles the cooldown up to
// MaxCooldownSeconds. The breaker only reorders candidates: when every
// candidate is cooling down, channels are selected as if it were off.
type ChannelCircuitSetting struct {
	Enabled            bool `json:"enabled"`
	FailureThreshold   int  `json:"failure_threshold"`
	CooldownSeconds    int  `json:"cooldown_seconds"`
	MaxCooldownSeconds int  `json:"max_cooldown_seconds"`
}

var channelCircuitSetting = ChannelCircuitSetting{
	Enabled:            true,
	FailureThreshold:   3,
	CooldownSeconds:    30,
	MaxCooldownSeconds: 300,
}

func init() {
	config.GlobalConfig.Register("channel_circuit_setting", &channelCircuitSetting)
}

func GetChannelCircuitSetting() *ChannelCircuitSetting {
	return &channelCircuitSetting
}
