package config

import "time"

// IsPeakUTC reports whether t falls in a peak pricing window. Peak windows are
// Monday through Friday, 01:00-04:00 UTC and 06:00-10:00 UTC. Intervals are
// half-open: [01:00, 04:00) and [06:00, 10:00).
func IsPeakUTC(t time.Time) bool {
	t = t.UTC()
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	hour := t.Hour()
	return (hour >= 1 && hour < 4) || (hour >= 6 && hour < 10)
}

// EstimateCost estimates the USD cost of a request using the pricing entry for
// model. Unknown models cost 0; pricing is never hardcoded here. Token counts
// are absolute token counts (not millions).
func (c *Config) EstimateCost(model string, cacheHitTokens, cacheMissTokens, completionTokens int, now time.Time) float64 {
	if c == nil {
		return 0
	}
	pricing, ok := c.Pricing[model]
	if !ok {
		return 0
	}

	hitRate := pricing.OffPeakCacheHitPerM
	missRate := pricing.OffPeakCacheMissPerM
	outputRate := pricing.OffPeakOutputPerM
	if IsPeakUTC(now) {
		hitRate = pricing.PeakCacheHitPerM
		missRate = pricing.PeakCacheMissPerM
		outputRate = pricing.PeakOutputPerM
	}

	return float64(cacheHitTokens)/1_000_000*hitRate +
		float64(cacheMissTokens)/1_000_000*missRate +
		float64(completionTokens)/1_000_000*outputRate
}
