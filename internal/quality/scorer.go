package quality

import (
	"math"
	"net"
	"strings"
)

const (
	vendorWeight     = 30
	identityWeight   = 15
	riskWeight       = 20
	confidenceWeight = 5
)

type QualityScoreResult struct {
	VendorReachability  int
	IdentityConsistency int
	RiskScore           int
	DataConfidence      int
	Score               int
	Confidence          int
	Complete            bool
	Errors              []ReportError
}

type IdentityScoreResult struct {
	Score                   int
	ConsensusIP             string
	Complete                bool
	Conflict                bool
	Available               int
	Consensus               int
	ASNConsistency          int
	RegionConsistency       int
	OrganizationConsistency int
}

type RiskScoreResult struct {
	Score      int
	Complete   bool
	Available  int
	RiskyVotes int
}

// ScoreQuality calculates a 0-100 quality score. Components are normalized
// before applying the fixed 30/15/20/5 weights, so a missing component is not
// silently converted into a perfect score.
func ScoreQuality(vendors map[string]VendorResult, sources, risks []SourceEvidence) QualityScoreResult {
	identity := ScoreIdentity(sources)
	risk := ScoreRisk(risks)
	vendor, vendorComplete := scoreVendors(vendors)

	dataConfidence := scoreDataConfidence(vendorComplete, identity, risk, len(sources) > 0 || len(risks) > 0)
	complete := vendorComplete && identity.Complete && risk.Complete
	weighted := 0.0
	availableWeight := 0
	if len(vendors) > 0 {
		weighted += float64(vendor) * vendorWeight
		availableWeight += vendorWeight
	}
	if identity.Available > 0 {
		weighted += float64(identity.Score) * identityWeight
		availableWeight += identityWeight
	}
	if risk.Available > 0 {
		weighted += float64(risk.Score) * riskWeight
		availableWeight += riskWeight
	}
	if len(vendors) > 0 || identity.Available > 0 || risk.Available > 0 {
		weighted += float64(dataConfidence) * confidenceWeight
		availableWeight += confidenceWeight
	}
	qualityScore := 0
	if availableWeight > 0 {
		qualityScore = roundClamp(weighted / float64(availableWeight))
	}
	result := QualityScoreResult{
		VendorReachability:  vendor,
		IdentityConsistency: identity.Score,
		RiskScore:           risk.Score,
		DataConfidence:      dataConfidence,
		Score:               qualityScore,
		Confidence:          roundClamp(confidenceEstimate(vendorComplete, identity, risk, dataConfidence)),
		Complete:            complete,
	}
	if identity.Conflict {
		result.Errors = append(result.Errors, ReportError{Code: ErrorIPConflict, Source: "identity", Message: "IP sources disagree"})
	}
	return result
}

// CalculateQualityScore is a descriptive alias for callers that prefer a
// verb-style API.
func CalculateQualityScore(vendors map[string]VendorResult, sources, risks []SourceEvidence) QualityScoreResult {
	return ScoreQuality(vendors, sources, risks)
}

func ScoreIdentity(sources []SourceEvidence) IdentityScoreResult {
	counts := make(map[string]int)
	families := make(map[string]string)
	available := 0
	for _, source := range sources {
		if !source.Available {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(source.IP))
		if ip == nil {
			continue
		}
		available++
		canonical := ip.String()
		counts[canonical]++
		families[canonical] = firstNonEmpty(source.IPFamily, ipFamily(canonical))
	}
	result := IdentityScoreResult{Available: available}
	for ip, count := range counts {
		if count > result.Consensus {
			result.Consensus = count
			result.ConsensusIP = ip
		}
	}
	if result.Consensus >= 2 {
		result.Complete = true
		// A single dissenting source is tolerated by the required two-of-three
		// rule; it remains visible in evidence but does not cause a switch.
	} else if len(counts) > 1 {
		result.Conflict = true
		result.Score = 0
		return result
	} else if result.Consensus == 1 {
		result.Score = 50
		return result
	} else {
		return result
	}

	result.ASNConsistency = consistencyScore(sources, func(source SourceEvidence) string { return source.ASN })
	result.RegionConsistency = consistencyScore(sources, func(source SourceEvidence) string { return source.Country })
	result.OrganizationConsistency = consistencyScore(sources, func(source SourceEvidence) string { return source.Organization })
	components := []int{100}
	for _, value := range []int{result.ASNConsistency, result.RegionConsistency, result.OrganizationConsistency} {
		if value >= 0 {
			components = append(components, value)
		}
	}
	total := 0
	for _, value := range components {
		total += value
	}
	result.Score = clampScore((total + len(components)/2) / len(components))
	return result
}

func consistencyScore(sources []SourceEvidence, value func(SourceEvidence) string) int {
	counts := make(map[string]int)
	available := 0
	for _, source := range sources {
		if !source.Available {
			continue
		}
		text := strings.ToLower(strings.TrimSpace(value(source)))
		if text == "" {
			continue
		}
		available++
		counts[text]++
	}
	if available == 0 {
		return -1
	}
	best := 0
	for _, count := range counts {
		if count > best {
			best = count
		}
	}
	if available == 1 {
		return 50
	}
	if best*2 <= available {
		return 0
	}
	return roundClamp(float64(best) * 100 / float64(available))
}

func ScoreRisk(sources []SourceEvidence) RiskScoreResult {
	result := RiskScoreResult{}
	cleanVotes := 0
	riskyVotes := 0
	for _, source := range sources {
		if !source.Available {
			continue
		}
		riskKnown := false
		risky := false
		for _, value := range []*bool{source.Proxy, source.VPN, source.Tor, source.Hosting, source.Blacklisted} {
			if value != nil {
				riskKnown = true
				if *value {
					risky = true
				}
			}
		}
		if source.AbuseScore != nil {
			riskKnown = true
			if *source.AbuseScore >= 50 {
				risky = true
			}
		}
		if !riskKnown {
			continue
		}
		result.Available++
		if risky {
			riskyVotes++
		} else {
			cleanVotes++
		}
	}
	result.RiskyVotes = riskyVotes
	if result.Available == 0 {
		return result
	}
	if riskyVotes > cleanVotes {
		result.Score = 0
	} else if cleanVotes > riskyVotes {
		result.Score = 100
	} else {
		result.Score = 50
	}
	result.Complete = result.Available >= 2 && cleanVotes != riskyVotes
	return result
}

func scoreVendors(vendors map[string]VendorResult) (score int, complete bool) {
	if len(vendors) == 0 {
		return 0, false
	}
	reachable := 0
	for _, vendor := range vendors {
		if vendorReachable(vendor) {
			reachable++
		}
	}
	return roundClamp(float64(reachable) * 100 / float64(len(vendors))), reachable == len(vendors)
}

func vendorReachable(result VendorResult) bool {
	if len(result.StatusCodes) > 0 {
		for _, status := range result.StatusCodes {
			if status >= 200 && status < 500 {
				return true
			}
		}
		return false
	}
	for _, status := range result.StatusCodes {
		if status >= 200 && status < 500 {
			return true
		}
	}
	return result.Reachable
}

func scoreDataConfidence(vendorsComplete bool, identity IdentityScoreResult, risk RiskScoreResult, anyEvidence bool) int {
	score := 0
	if vendorsComplete {
		score += 40
	} else if anyEvidence {
		score += 20
	}
	if identity.Complete {
		score += 35
	} else if identity.Available > 0 {
		score += 15
	}
	if risk.Complete {
		score += 25
	} else if risk.Available > 0 {
		score += 10
	}
	return score
}

func confidenceEstimate(vendorsComplete bool, identity IdentityScoreResult, risk RiskScoreResult, data int) float64 {
	confidence := float64(data)
	if vendorsComplete {
		confidence += 10
	}
	if identity.Conflict {
		confidence -= 25
	}
	if risk.Available == 1 {
		confidence -= 10
	}
	return confidence
}

// EffectiveScore combines normalized component scores using the fixed 70/30
// quality/stability split. Inputs are clamped before the final rounding.
func EffectiveScore(quality, stability int) int {
	return roundClamp(float64(clampScore(quality))*0.70 + float64(clampScore(stability))*0.30)
}

// BaselineDropped is deliberately inclusive: a score exactly 20 points below
// the immutable baseline is the first value eligible for revalidation.
func BaselineDropped(baseline, current, threshold int) bool {
	if threshold <= 0 {
		threshold = 20
	}
	return baseline-current >= threshold
}

func clampScore(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func roundClamp(value float64) int {
	if math.IsNaN(value) || value <= 0 {
		return 0
	}
	if value >= 100 {
		return 100
	}
	return clampScore(int(math.Round(value)))
}

// ScoreReport applies the score model to a report while preserving all raw
// evidence and typed errors collected by the caller.
func ScoreReport(report Report) Report {
	quality := ScoreQuality(report.VendorResults, report.SourceEvidence, report.RiskEvidence)
	report.QualityScore = quality.Score
	report.EffectiveScore = EffectiveScore(report.QualityScore, report.StabilityScore)
	report.ConfidencePercent = quality.Confidence
	report.Complete = report.Complete && quality.Complete
	report.Errors = append(report.Errors, quality.Errors...)
	return report
}
