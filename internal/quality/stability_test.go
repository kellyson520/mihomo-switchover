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
	if got.CoveragePercent != 1 || got.AvailabilityPercent != 75 {
		t.Fatalf("coverage/availability=%d/%d, want 1/75", got.CoveragePercent, got.AvailabilityPercent)
	}
	if got.StabilityScore <= 0 || got.StabilityScore > 100 {
		t.Fatalf("stability score=%d, want clamped positive score", got.StabilityScore)
	}
}

func TestAggregateStabilityDoesNotTreatHourlySummaryAsSamplingInterval(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	proxy := mihomo.Proxy{Name: "node-a", Alive: true, History: []mihomo.DelayHistory{
		{Time: now.Add(-5 * time.Minute), Delay: 100},
		{Time: now.Add(-10 * time.Minute), Delay: 100},
		{Time: now.Add(-15 * time.Minute), Delay: 100},
	}}

	got := AggregateStability([]mihomo.Proxy{proxy}, "node-a", now, stabilityTestConfig())
	if got.ExpectedSamples != 288 {
		t.Fatalf("expected samples=%d, want 288 five-minute samples in 24h", got.ExpectedSamples)
	}
	if got.CoveragePercent >= 10 {
		t.Fatalf("coverage=%d, want conservative low coverage", got.CoveragePercent)
	}
	if got.StabilityScore >= 100 {
		t.Fatalf("stability score=%d, low coverage must not receive full score", got.StabilityScore)
	}
}

func TestAggregateStabilityPenalizesMaxLatencySpike(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	cleanHistory := make([]mihomo.DelayHistory, 0, 20)
	spikeHistory := make([]mihomo.DelayHistory, 0, 20)
	for index := 1; index <= 20; index++ {
		sample := mihomo.DelayHistory{Time: now.Add(-time.Duration(index) * 5 * time.Minute), Delay: 100}
		cleanHistory = append(cleanHistory, sample)
		spikeHistory = append(spikeHistory, sample)
	}
	spikeHistory[0].Delay = 5000

	cfg := stabilityTestConfig()
	cfg.HistoryWindow = 2 * time.Hour
	clean := AggregateStability([]mihomo.Proxy{{Name: "node-a", History: cleanHistory}}, "node-a", now, cfg)
	spike := AggregateStability([]mihomo.Proxy{{Name: "node-a", History: spikeHistory}}, "node-a", now, cfg)
	if clean.MaxMS != 100 || spike.MaxMS != 5000 {
		t.Fatalf("max latency clean/spike=%d/%d, want 100/5000", clean.MaxMS, spike.MaxMS)
	}
	if spike.StabilityScore >= clean.StabilityScore {
		t.Fatalf("stability score clean/spike=%d/%d, max latency spike must be penalized", clean.StabilityScore, spike.StabilityScore)
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
