package quality

import (
	"sort"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
)

// AggregateStability only consumes mihomo's already collected history. It
// never calls the mihomo API and never creates an external request, making it
// safe to run from the hourly summary loop while production traffic continues.
func AggregateStability(proxies []mihomo.Proxy, node string, now time.Time, cfg config.QualityStabilityConfig) StabilitySnapshot {
	now = now.UTC()
	window := cfg.HistoryWindow
	if window <= 0 {
		window = 24 * time.Hour
	}
	start := now.Add(-window)
	staleAfter := cfg.StaleAfter
	if staleAfter <= 0 {
		staleAfter = window
	}
	result := StabilitySnapshot{
		Identity:    NodeKey{Node: node},
		ObservedAt:  now,
		WindowStart: start,
		WindowEnd:   now,
	}

	var history []mihomo.DelayHistory
	for _, proxy := range proxies {
		if proxy.Name == node {
			history = append(history, proxy.History...)
			break
		}
	}
	filtered := make([]mihomo.DelayHistory, 0, len(history))
	for _, sample := range history {
		sampleTime := sample.Time.UTC()
		if sampleTime.Before(start) || sampleTime.After(now) {
			continue
		}
		if now.Sub(sampleTime) > staleAfter {
			continue
		}
		filtered = append(filtered, sample)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Time.Before(filtered[j].Time) })
	result.Samples = len(filtered)
	if len(filtered) > 0 {
		result.LastSampleAt = filtered[len(filtered)-1].Time.UTC()
		result.Fresh = now.Sub(result.LastSampleAt) <= staleAfter
	}
	result.ExpectedSamples = expectedSamples(window, cfg.SummaryInterval)
	if result.ExpectedSamples <= 0 {
		return result
	}
	if result.ExpectedSamples > 0 {
		result.CoveragePercent = clampScore(int((float64(result.Samples) * 100 / float64(result.ExpectedSamples)) + 0.5))
	}
	for _, sample := range filtered {
		if sample.Delay > 0 {
			result.AliveSamples++
		}
	}
	if result.Samples > 0 {
		result.AvailabilityPercent = clampScore(int((float64(result.AliveSamples) * 100 / float64(result.Samples)) + 0.5))
	}
	if cfg.MinimumSamples < 1 {
		cfg.MinimumSamples = 3
	}
	minimumCoverage := cfg.MinimumCoverage
	if minimumCoverage < 1 {
		minimumCoverage = 10
	}
	latencies := make([]int, 0, result.AliveSamples)
	for _, sample := range filtered {
		if sample.Delay > 0 {
			latencies = append(latencies, sample.Delay)
		}
	}
	if len(latencies) == 0 {
		return result
	}
	sort.Ints(latencies)
	result.P50MS = percentileNearestRank(latencies, 0.50)
	result.P95MS = percentileNearestRank(latencies, 0.95)
	result.MaxMS = latencies[len(latencies)-1]
	result.JitterMS = result.P95MS - result.P50MS
	if result.JitterMS < 0 {
		result.JitterMS = 0
	}
	if len(filtered) < cfg.MinimumSamples || result.CoveragePercent < minimumCoverage || !result.Fresh {
		return result
	}
	result.Known = true
	result.StabilityScore = calculateStabilityScore(result, cfg)
	return result
}

func expectedSamples(window, interval time.Duration) int {
	if window <= 0 {
		return 0
	}
	// SummaryInterval is the reporting cadence, not mihomo's per-node delay
	// probe cadence. Mihomo's native default is five minutes; use that as the
	// conservative lower bound so an hourly summary cannot inflate coverage.
	const mihomoDelayInterval = 5 * time.Minute
	if interval <= 0 || interval > mihomoDelayInterval {
		interval = mihomoDelayInterval
	}
	value := int(window / interval)
	if window%interval != 0 {
		value++
	}
	if value < 1 {
		return 1
	}
	return value
}

func percentileNearestRank(values []int, percentile float64) int {
	if len(values) == 0 {
		return 0
	}
	index := int(float64(len(values)) * percentile)
	if float64(index) < float64(len(values))*percentile {
		index++
	}
	if index < 1 {
		index = 1
	}
	if index > len(values) {
		index = len(values)
	}
	return values[index-1]
}

func calculateStabilityScore(snapshot StabilitySnapshot, cfg config.QualityStabilityConfig) int {
	good := cfg.GoodLatencyMS
	bad := cfg.BadLatencyMS
	if good <= 0 {
		good = 500
	}
	if bad <= good {
		bad = good + 1
	}
	latencyComponent := latencyScore(snapshot.P50MS, good, bad)
	jitterScore := latencyScore(snapshot.JitterMS, 0, bad-good)
	peakScore := latencyScore(snapshot.MaxMS, good, bad)
	// A single severe peak is meaningful even when p50/p95 look healthy. Keep
	// it in the volatility component instead of changing the documented
	// availability/latency/volatility weights.
	volatilityScore := roundClamp(float64(jitterScore)*0.60 + float64(peakScore)*0.40)
	value := float64(snapshot.AvailabilityPercent)*0.50 +
		float64(latencyComponent)*0.30 +
		float64(volatilityScore)*0.20
	return roundClamp(value * float64(snapshot.CoveragePercent) / 100)
}

func latencyScore(value, good, bad int) int {
	if value <= good {
		return 100
	}
	if value >= bad {
		return 0
	}
	return clampScore(int((float64(bad-value) * 100 / float64(bad-good)) + 0.5))
}
