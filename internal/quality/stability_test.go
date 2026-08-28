package quality

import (
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
)

func stabilityTestConfig() config.QualityStabilityConfig {
	return config.QualityStabilityConfig{
		SummaryInterval: time.Hour,
		HistoryWindow:   24 * time.Hour,
		MinimumSamples:  3,
		StaleAfter:      2 * time.Hour,
		GoodLatencyMS:   100,
		BadLatencyMS:    1000,
	}
}

func TestAggregateStabilityUsesFreshHistoryAndComputesPercentiles(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	proxy := mihomo.Proxy{
		Name:  "node-a",
		Alive: true,
		History: []mihomo.DelayHistory{
			{Time: now.Add(-30 * time.Minute), Delay: 100},
			{Time: now.Add(-60 * time.Minute), Delay: 200},
			{Time: now.Add(-90 * time.Minute), Delay: 300},
			{Time: now.Add(-2 * time.Hour), Delay: 0},
			{Time: now.Add(-3 * time.Hour), Delay: 900}, // stale and excluded
		},
	}

	got := AggregateStability([]mihomo.Proxy{proxy}, "node-a", now, stabilityTestConfig())
	if !got.Known || !got.Fresh || got.Samples != 4 || got.AliveSamples != 3 {
		t.Fatalf("snapshot=%+v, want fresh four-sample history with one failed sample", got)
	}
	if got.P50MS != 200 || got.P95MS != 300 || got.MaxMS != 300 || got.JitterMS != 100 {
		t.Fatalf("percentiles=%+v, want p50=200 p95=300 max=300 jitter=100", got)
	}
	if got.CoveragePercent != 17 || got.AvailabilityPercent != 75 {
		t.Fatalf("coverage/availability=%d/%d, want 17/75", got.CoveragePercent, got.AvailabilityPercent)
	}
	if got.StabilityScore <= 0 || got.StabilityScore > 100 {
		t.Fatalf("stability score=%d, want clamped positive score", got.StabilityScore)
	}
}

func TestAggregateStabilityReturnsUnknownForInsufficientOrStaleHistory(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	proxy := mihomo.Proxy{Name: "node-a", History: []mihomo.DelayHistory{
		{Time: now.Add(-3 * time.Hour), Delay: 100},
		{Time: now.Add(-4 * time.Hour), Delay: 100},
	}}
	got := AggregateStability([]mihomo.Proxy{proxy}, "node-a", now, stabilityTestConfig())
	if got.Known || got.Fresh || got.StabilityScore != 0 {
		t.Fatalf("snapshot=%+v, want unknown stale history", got)
	}

	proxy.History = []mihomo.DelayHistory{{Time: now.Add(-30 * time.Minute), Delay: 100}}
	got = AggregateStability([]mihomo.Proxy{proxy}, "node-a", now, stabilityTestConfig())
	if got.Known || got.Samples != 1 {
		t.Fatalf("snapshot=%+v, want unknown below minimum samples", got)
	}
}

func TestAggregateStabilityDoesNotIssueRequestsOrDelayProbes(t *testing.T) {
	got := AggregateStability(nil, "missing", time.Now(), stabilityTestConfig())
	if got.Known || got.Samples != 0 {
		t.Fatalf("snapshot=%+v, want pure unknown result", got)
	}
}
