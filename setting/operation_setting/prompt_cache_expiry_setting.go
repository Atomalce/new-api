package operation_setting

import "github.com/QuantumNous/new-api/setting/config"

// Clamp bounds for the admin-configured cycle length. The billing settings UI
// mirrors the same numbers in its form validation.
const (
	PromptCacheExpiryMinCycleSeconds = 10
	PromptCacheExpiryMaxCycleSeconds = 86400
)

// PromptCacheExpirySetting controls the Codex /v1/responses prompt-cache
// discount expiry billing policy (service/prompt_cache_expiry.go): the
// operator switch and the cycle length used as the Redis claim TTL.
type PromptCacheExpirySetting struct {
	Enabled      bool `json:"enabled"`
	CycleSeconds int  `json:"cycle_seconds"`
}

// Defaults preserve the behavior originally shipped with the policy:
// enabled, 60-second cycle.
var promptCacheExpirySetting = PromptCacheExpirySetting{
	Enabled:      true,
	CycleSeconds: 60,
}

func init() {
	config.GlobalConfig.Register("prompt_cache_expiry_setting", &promptCacheExpirySetting)
}

func GetPromptCacheExpirySetting() *PromptCacheExpirySetting {
	return &promptCacheExpirySetting
}

// EffectiveCycleSeconds clamps the configured cycle length so a misconfigured
// option can never become a zero/negative Redis TTL or an effectively
// permanent cycle.
func (s *PromptCacheExpirySetting) EffectiveCycleSeconds() int {
	if s.CycleSeconds < PromptCacheExpiryMinCycleSeconds {
		return PromptCacheExpiryMinCycleSeconds
	}
	if s.CycleSeconds > PromptCacheExpiryMaxCycleSeconds {
		return PromptCacheExpiryMaxCycleSeconds
	}
	return s.CycleSeconds
}
