package quality

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
)

var (
	ErrRecommendationInvalid  = errors.New("quality recommendation invalid")
	ErrRecommendationStale    = errors.New("quality recommendation stale")
	ErrRecommendationMismatch = errors.New("quality recommendation identity mismatch")
	ErrRecommendationUnsafe   = errors.New("quality recommendation unsafe")
)

const defaultRecommendationMaxAge = 720 * time.Hour

// RecommendationValidation describes the live facts that must agree with a
// persisted recommendation before it can influence a production group. The
// caller supplies the current IP because mihomo's proxy metadata does not
// expose an exit address.
type RecommendationValidation struct {
	Target                  config.QualityTarget
	CurrentNode             string
	CurrentIP               string
	CurrentProvider         string
	ProviderAlive           bool
	ProviderHistoryFresh    bool
	VendorConnectivityFresh bool
	Now                     time.Time
	MaxAge                  time.Duration
	MinimumScore            int
	MinimumConfidence       int
}

// ReplacementRequest is the complete evidence needed to release a sticky
// node. Sticky and candidate validation are intentionally separate: the
// sticky node may have a low score, but its report still has to be complete
// and its live identity and provider evidence must be trustworthy.
type ReplacementRequest struct {
	Sticky              Recommendation
	Candidate           Recommendation
	StickyValidation    RecommendationValidation
	CandidateValidation RecommendationValidation
	Thresholds          config.QualityThresholds
}

// GenerateRecommendation turns a scored report and its persisted identity
// record into an auditable recommendation. It does not change the immutable
// baseline: the record is the source of truth for that value.
func GenerateRecommendation(report Report, record NodeRecord, target config.QualityTarget, now time.Time) (Recommendation, error) {
	if err := report.Validate(); err != nil {
		return Recommendation{}, fmt.Errorf("%w: report: %v", ErrRecommendationInvalid, err)
	}
	key := report.Identity.Canonical()
	if strings.TrimSpace(target.ID) == "" || key.Target != target.ID {
		return Recommendation{}, fmt.Errorf("%w: target %q does not match report identity", ErrRecommendationMismatch, target.ID)
	}
	if target.SourceGroup == "" || target.Provider == "" {
		return Recommendation{}, fmt.Errorf("%w: target source group and provider are required", ErrRecommendationInvalid)
	}
	if key.Provider != target.Provider {
		return Recommendation{}, fmt.Errorf("%w: provider %q does not match target provider %q", ErrRecommendationMismatch, key.Provider, target.Provider)
	}
	if key.Node == "" {
		return Recommendation{}, fmt.Errorf("%w: node is empty", ErrRecommendationInvalid)
	}

	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	reportedAt := report.ObservedAt.UTC()
	if reportedAt.IsZero() {
		reportedAt = now
	}
	baseline := report.EffectiveScore
	if record.Baseline != nil {
		if record.Baseline.Identity.ID() != key.ID() {
			return Recommendation{}, fmt.Errorf("%w: record baseline identity differs from report", ErrRecommendationMismatch)
		}
		baseline = record.Baseline.Score
	}

	recommendation := Recommendation{
		ID:                   key.ID(),
		Target:               target.ID,
		SourceGroup:          target.SourceGroup,
		Provider:             target.Provider,
		Node:                 key.Node,
		Identity:             key,
		ReportedAt:           reportedAt,
		EffectiveScore:       clampRecommendationScore(report.EffectiveScore),
		BaselineScore:        clampRecommendationScore(baseline),
		QualityScore:         clampRecommendationScore(report.QualityScore),
		StabilityScore:       clampRecommendationScore(report.StabilityScore),
		ConfidencePercent:    clampRecommendationScore(report.ConfidencePercent),
		Complete:             report.Complete,
		Connected:            report.Eligible || reportHasReachableVendor(report),
		ProviderAlive:        report.ProviderAlive || report.Provider.Alive,
		ProviderHistoryFresh: report.ProviderHistoryFresh || report.Provider.HistoryFresh,
		CreatedAt:            now,
		Reason:               recommendationReason(report),
	}
	return recommendation, nil
}

// BuildRecommendation and NewRecommendation are descriptive aliases kept
// for callers that use either builder terminology or constructor terminology.
func BuildRecommendation(report Report, record NodeRecord, target config.QualityTarget, now time.Time) (Recommendation, error) {
	return GenerateRecommendation(report, record, target, now)
}

func NewRecommendation(report Report, record NodeRecord, target config.QualityTarget, now time.Time) (Recommendation, error) {
	return GenerateRecommendation(report, record, target, now)
}

func recommendationReason(report Report) string {
	if report.Eligible {
		return "eligible quality and stability report"
	}
	if len(report.Errors) == 0 {
		return "report is not eligible for production selection"
	}
	parts := make([]string, 0, len(report.Errors))
	for _, item := range report.Errors {
		if code := strings.TrimSpace(string(item.Code)); code != "" {
			parts = append(parts, code)
		}
	}
	if len(parts) == 0 {
		return "report is not eligible for production selection"
	}
	return "report rejected: " + strings.Join(parts, ",")
}

func reportHasReachableVendor(report Report) bool {
	if len(report.VendorResults) == 0 {
		return false
	}
	for _, vendor := range report.VendorResults {
		if !vendor.Reachable {
			return false
		}
	}
	return true
}

func clampRecommendationScore(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// ValidateRecommendation performs the fail-closed validation used by the
// realtime service. CurrentIP is deliberately required: a recommendation
// with an old IP must never be treated as the current provider identity.
func ValidateRecommendation(recommendation Recommendation, validation RecommendationValidation) error {
	if err := validateRecommendationShape(recommendation); err != nil {
		return err
	}
	if err := recommendationIdentityMatches(recommendation, validation); err != nil {
		return err
	}
	now := validation.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if recommendation.ReportedAt.IsZero() || recommendation.ReportedAt.After(now.Add(2*time.Minute)) {
		return fmt.Errorf("%w: report time is missing or in the future", ErrRecommendationStale)
	}
	if !recommendation.ExpiresAt.IsZero() && now.After(recommendation.ExpiresAt) {
		return fmt.Errorf("%w: expires at %s", ErrRecommendationStale, recommendation.ExpiresAt.UTC().Format(time.RFC3339))
	}
	maxAge := validation.MaxAge
	if maxAge <= 0 {
		maxAge = defaultRecommendationMaxAge
	}
	if now.Sub(recommendation.ReportedAt) > maxAge {
		return fmt.Errorf("%w: age %s exceeds %s", ErrRecommendationStale, now.Sub(recommendation.ReportedAt).Round(time.Second), maxAge)
	}

	minimumScore := validation.MinimumScore
	if minimumScore <= 0 {
		minimumScore = 60
	}
	minimumConfidence := validation.MinimumConfidence
	if minimumConfidence <= 0 {
		minimumConfidence = 70
	}
	if recommendation.EffectiveScore < minimumScore {
		return fmt.Errorf("%w: effective score %d is below %d", ErrRecommendationUnsafe, recommendation.EffectiveScore, minimumScore)
	}
	if recommendation.ConfidencePercent < minimumConfidence {
		return fmt.Errorf("%w: confidence %d is below %d", ErrRecommendationUnsafe, recommendation.ConfidencePercent, minimumConfidence)
	}
	if !recommendation.Complete || !recommendation.Connected {
		return fmt.Errorf("%w: report is incomplete or vendor connectivity is not fresh", ErrRecommendationUnsafe)
	}
	if !recommendation.ProviderAlive || !recommendation.ProviderHistoryFresh {
		return fmt.Errorf("%w: recommendation provider evidence is not healthy and fresh", ErrRecommendationUnsafe)
	}
	if !validation.ProviderAlive || !validation.ProviderHistoryFresh || !validation.VendorConnectivityFresh {
		return fmt.Errorf("%w: live provider or vendor evidence is not healthy and fresh", ErrRecommendationUnsafe)
	}
	return nil
}

func validateRecommendationShape(recommendation Recommendation) error {
	if err := recommendation.Identity.Validate(); err != nil {
		return fmt.Errorf("%w: identity: %v", ErrRecommendationInvalid, err)
	}
	if recommendation.Target == "" || recommendation.SourceGroup == "" || recommendation.Provider == "" || recommendation.Node == "" {
		return fmt.Errorf("%w: target, source group, provider, and node are required", ErrRecommendationInvalid)
	}
	if recommendation.Identity.Target != recommendation.Target || recommendation.Identity.Provider != recommendation.Provider || recommendation.Identity.Node != recommendation.Node {
		return fmt.Errorf("%w: routing fields differ from IP identity", ErrRecommendationMismatch)
	}
	for name, score := range map[string]int{
		"effective": recommendation.EffectiveScore, "baseline": recommendation.BaselineScore,
		"confidence": recommendation.ConfidencePercent,
	} {
		if score < 0 || score > 100 {
			return fmt.Errorf("%w: %s score is outside 0-100", ErrRecommendationInvalid, name)
		}
	}
	return nil
}

func recommendationIdentityMatches(recommendation Recommendation, validation RecommendationValidation) error {
	target := validation.Target
	if target.ID != "" && recommendation.Target != target.ID {
		return fmt.Errorf("%w: target %q does not match %q", ErrRecommendationMismatch, recommendation.Target, target.ID)
	}
	if target.SourceGroup != "" && recommendation.SourceGroup != target.SourceGroup {
		return fmt.Errorf("%w: source group %q does not match %q", ErrRecommendationMismatch, recommendation.SourceGroup, target.SourceGroup)
	}
	if target.Provider != "" && recommendation.Provider != target.Provider {
		return fmt.Errorf("%w: provider %q does not match %q", ErrRecommendationMismatch, recommendation.Provider, target.Provider)
	}
	if validation.CurrentNode == "" || recommendation.Node != validation.CurrentNode {
		return fmt.Errorf("%w: node %q is not current node %q", ErrRecommendationMismatch, recommendation.Node, validation.CurrentNode)
	}
	if validation.CurrentProvider == "" || recommendation.Provider != validation.CurrentProvider {
		return fmt.Errorf("%w: provider identity is not current provider", ErrRecommendationMismatch)
	}
	if validation.CurrentIP == "" || recommendation.Identity.IP != validation.CurrentIP {
		return fmt.Errorf("%w: IP %q is not current IP %q", ErrRecommendationMismatch, recommendation.Identity.IP, validation.CurrentIP)
	}
	return nil
}

// EvaluateReplacement applies sticky-first policy. A better candidate is
// ignored until the connected sticky identity has dropped by the inclusive
// baseline threshold and all revalidation gates pass.
func EvaluateReplacement(request ReplacementRequest) (bool, string) {
	thresholds := request.Thresholds
	if thresholds.BaselineDropPoints <= 0 {
		thresholds.BaselineDropPoints = 20
	}
	if thresholds.MinimumConfidence <= 0 {
		thresholds.MinimumConfidence = 70
	}
	if thresholds.CandidateMinimumScore <= 0 {
		thresholds.CandidateMinimumScore = 60
	}
	request.CandidateValidation.MinimumConfidence = thresholds.MinimumConfidence
	request.CandidateValidation.MinimumScore = thresholds.CandidateMinimumScore
	if err := ValidateRecommendation(request.Candidate, request.CandidateValidation); err != nil {
		return false, "candidate rejected: " + err.Error()
	}
	if err := validateStickyEvidence(request.Sticky, request.StickyValidation, thresholds.MinimumConfidence); err != nil {
		return false, "sticky retained: " + err.Error()
	}
	if !BaselineDropped(request.Sticky.BaselineScore, request.Sticky.EffectiveScore, thresholds.BaselineDropPoints) {
		return false, "sticky retained: baseline drop threshold not reached"
	}
	if request.Candidate.Node == request.Sticky.Node {
		return false, "sticky retained: candidate is the current node"
	}
	if request.Candidate.EffectiveScore <= request.Sticky.EffectiveScore {
		return false, "sticky retained: candidate is not better"
	}
	return true, "sticky released: inclusive baseline drop threshold reached"
}

// ValidateStickyRecommendation checks the evidence gates for the currently
// connected identity without applying the candidate minimum score. A sticky
// node is allowed to be below that candidate threshold precisely so an
// inclusive baseline drop can release it.
func ValidateStickyRecommendation(recommendation Recommendation, validation RecommendationValidation, minimumConfidence int) error {
	return validateStickyEvidence(recommendation, validation, minimumConfidence)
}

func validateStickyEvidence(sticky Recommendation, validation RecommendationValidation, minimumConfidence int) error {
	if err := validateRecommendationShape(sticky); err != nil {
		return err
	}
	if err := recommendationIdentityMatches(sticky, validation); err != nil {
		return err
	}
	if minimumConfidence <= 0 {
		minimumConfidence = 70
	}
	if !sticky.Complete || sticky.ConfidencePercent < minimumConfidence || !sticky.Connected {
		return fmt.Errorf("%w: sticky report is incomplete or below confidence", ErrRecommendationUnsafe)
	}
	if !sticky.ProviderAlive || !sticky.ProviderHistoryFresh || !validation.ProviderAlive || !validation.ProviderHistoryFresh || !validation.VendorConnectivityFresh {
		return fmt.Errorf("%w: sticky provider or vendor evidence is not healthy and fresh", ErrRecommendationUnsafe)
	}
	now := validation.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	maxAge := validation.MaxAge
	if maxAge <= 0 {
		maxAge = defaultRecommendationMaxAge
	}
	if sticky.ReportedAt.IsZero() || now.Sub(sticky.ReportedAt) > maxAge {
		return fmt.Errorf("%w: sticky report is stale", ErrRecommendationStale)
	}
	if sticky.ReportedAt.After(now.Add(2*time.Minute)) || (!sticky.ExpiresAt.IsZero() && now.After(sticky.ExpiresAt)) {
		return fmt.Errorf("%w: sticky report is expired or in the future", ErrRecommendationStale)
	}
	return nil
}

// ReadRecommendations reads only fresh, structurally valid records. A
// malformed persistence file is treated as an empty set because Store has
// already moved it aside as evidence; runtime must remain fail-closed and
// keep serving with its existing sticky decision.
func ReadRecommendations(store *Store, now time.Time, maxAge time.Duration) ([]Recommendation, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: recommendation store is missing", ErrRecommendationInvalid)
	}
	recommendations, err := store.LoadRecommendations()
	if err != nil {
		if errors.Is(err, ErrCorruptJSON) {
			return []Recommendation{}, nil
		}
		return nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if maxAge <= 0 {
		maxAge = defaultRecommendationMaxAge
	}
	result := make([]Recommendation, 0, len(recommendations))
	for _, recommendation := range recommendations {
		if err := validateRecommendationShape(recommendation); err != nil {
			continue
		}
		if recommendation.ReportedAt.IsZero() || recommendation.ReportedAt.After(now.Add(2*time.Minute)) {
			continue
		}
		if !recommendation.ExpiresAt.IsZero() && now.After(recommendation.ExpiresAt) {
			continue
		}
		if now.Sub(recommendation.ReportedAt) > maxAge {
			continue
		}
		result = append(result, recommendation)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Target != result[j].Target {
			return result[i].Target < result[j].Target
		}
		if result[i].EffectiveScore != result[j].EffectiveScore {
			return result[i].EffectiveScore > result[j].EffectiveScore
		}
		if !result[i].ReportedAt.Equal(result[j].ReportedAt) {
			return result[i].ReportedAt.After(result[j].ReportedAt)
		}
		return result[i].Node < result[j].Node
	})
	return result, nil
}

func LoadRecommendations(store *Store, now time.Time, maxAge time.Duration) ([]Recommendation, error) {
	return ReadRecommendations(store, now, maxAge)
}

func LoadFreshRecommendations(store *Store, now time.Time, maxAge time.Duration) ([]Recommendation, error) {
	return ReadRecommendations(store, now, maxAge)
}
