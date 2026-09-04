package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/decision"
	"mihomo-guardian/internal/logging"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/purity"
	"mihomo-guardian/internal/quality"
	"mihomo-guardian/internal/state"
)

type API interface {
	GetProxy(context.Context, string) (mihomo.Proxy, error)
	SetProxy(context.Context, string, string) error
	Delay(context.Context, string, string, time.Duration) (int, error)
}

type ProviderAPI interface {
	GetProvider(context.Context, string) (mihomo.Provider, error)
}

type ProviderHealthChecker interface {
	HealthCheckProvider(context.Context, string) error
}

type ExternalProbe interface {
	Check(context.Context, config.ProbeSpec) probe.Result
}

type ExternalFetcher interface {
	Fetch(context.Context, config.ProbeSpec) (probe.Result, []byte)
}

type Service struct {
	cfg                  config.Config
	api                  API
	external             ExternalProbe
	store                *state.Store
	logger               *logging.Logger
	state                state.State
	loaded               bool
	engine               *decision.Engine
	quality              *quality.Store
	recs                 []quality.Recommendation
	probe                cachedProbeResult
	purity               cachedPurityResult
	providerHealthChecks map[string]time.Time
	clock                func() time.Time
	hintMu               sync.Mutex
	probeHintSequence    uint64
}

type cachedProbeResult struct {
	Key             string
	CheckedAt       time.Time
	Healthy         bool
	FailureEligible bool
	RouteRejected   bool
	HintSequence    uint64
}

type probeSummary struct {
	Healthy         bool
	FailureEligible bool
	RouteRejected   bool
}

type cachedPurityResult struct {
	Key       string
	CheckedAt time.Time
	Result    purity.Result
}

func NewService(cfg config.Config, api API, external ExternalProbe, store *state.Store, logger *logging.Logger, qualityStores ...*quality.Store) *Service {
	service := &Service{
		cfg: cfg, api: api, external: external, store: store, logger: logger,
		engine: newDecisionEngine(cfg), providerHealthChecks: make(map[string]time.Time),
	}
	if len(qualityStores) > 0 {
		service.quality = qualityStores[0]
	}
	return service
}

func NewServiceWithQualityStore(cfg config.Config, api API, external ExternalProbe, store *state.Store, logger *logging.Logger, qualityStore *quality.Store) *Service {
	return NewService(cfg, api, external, store, logger, qualityStore)
}

func (s *Service) SetQualityStore(store *quality.Store) {
	s.quality = store
	s.recs = nil
}

func (s *Service) nowUTC() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// ObserveMihomoError accepts only the redacted category emitted by the
// mihomo log watcher. A network hint invalidates the current public probe
// cache, but never creates a switch action by itself.
func (s *Service) ObserveMihomoError(hint mihomo.LogHint) {
	if hint.Category != mihomo.LogHintNetwork {
		return
	}
	s.hintMu.Lock()
	s.probeHintSequence++
	sequence := s.probeHintSequence
	s.hintMu.Unlock()
	s.log("mihomo_error_hint", map[string]any{"category": string(hint.Category), "sequence": sequence})
}

func (s *Service) currentProbeHintSequence() uint64 {
	s.hintMu.Lock()
	defer s.hintMu.Unlock()
	return s.probeHintSequence
}

func newDecisionEngine(cfg config.Config) *decision.Engine {
	return decision.NewEngine(decision.DecisionConfig{
		MainChannel:            cfg.Groups.Main,
		BackupChannel:          cfg.Groups.Backup,
		FailuresBeforeSwitch:   cfg.Decision.FailuresBeforeSwitch,
		RecoveriesBeforeSwitch: cfg.Decision.RecoveriesBeforeSwitch,
		MinHold:                cfg.Decision.MinHold,
	})
}

func (s *Service) UpdateConfig(cfg config.Config) {
	s.cfg = cfg
	s.engine = newDecisionEngine(cfg)
	s.recs = nil
	// A changed probe/purity configuration must receive fresh public evidence.
	s.probe = cachedProbeResult{}
	s.purity = cachedPurityResult{}
	s.providerHealthChecks = make(map[string]time.Time)
}

func (s *Service) RunCycle(ctx context.Context) error {
	loaded, err := s.store.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.state, s.loaded = loaded, true
	if err := s.refreshRecommendations(ctx); err != nil {
		return err
	}

	channel, err := s.api.GetProxy(ctx, s.cfg.Groups.Channel)
	if err != nil {
		return fmt.Errorf("read channel group: %w", err)
	}
	if channel.Now != s.cfg.Groups.Main && channel.Now != s.cfg.Groups.Backup {
		return fmt.Errorf("unexpected channel selection %q; refusing automatic decision", channel.Now)
	}
	if s.state.CurrentChannel != channel.Now {
		s.state.CurrentChannel = channel.Now
		s.state.FailureStreak = 0
		s.state.RecoveryStreak = 0
		s.log("channel_state_synced", map[string]any{"channel": channel.Now})
	}

	mainGroup, err := s.api.GetProxy(ctx, s.cfg.Groups.Main)
	if err != nil {
		return fmt.Errorf("read main group: %w", err)
	}
	backupGroup, err := s.api.GetProxy(ctx, s.cfg.Groups.Backup)
	if err != nil {
		return fmt.Errorf("read backup group: %w", err)
	}

	mainHealthy, backupHealthy := false, false
	mainFailureEligible := false
	currentHealthySample := false
	activeProbeKey := ""
	if s.state.CurrentChannel == s.cfg.Groups.Main {
		mainNode, mainProviderHealthy := s.ensureProvider(ctx, "main", s.cfg.Groups.Main, mainGroup)
		probeNode := mainNode
		if probeNode == "" {
			probeNode = mainGroup.Now
		}
		activeProbeKey = probeKey(s.cfg.Groups.Main, probeNode)
		activeHealthy, sampled, failureEligible, routeRejected := s.activeHealthy(ctx, activeProbeKey)
		if sampled && routeRejected {
			if replacementNode, replacement, ok := s.findCompatibleProviderNode(ctx, "main", s.cfg.Groups.Main, mainGroup, mainNode); ok {
				mainNode = replacementNode
				activeProbeKey = probeKey(s.cfg.Groups.Main, mainNode)
				activeHealthy = replacement.Healthy
				failureEligible = replacement.FailureEligible
			}
		}
		mainHealthy = mainProviderHealthy && activeHealthy
		mainFailureEligible = failureEligible
		currentHealthySample = sampled
		if !activeHealthy {
			_, backupHealthy = s.ensureProvider(ctx, "backup", s.cfg.Groups.Backup, backupGroup)
		}
	} else if s.state.CurrentChannel == s.cfg.Groups.Backup {
		backupNode, _ := s.ensureProvider(ctx, "backup", s.cfg.Groups.Backup, backupGroup)
		activeProbeKey = probeKey(s.cfg.Groups.Backup, backupNode)
		_, sampled, _, routeRejected := s.activeHealthy(ctx, activeProbeKey)
		if sampled && routeRejected {
			if replacementNode, _, ok := s.findCompatibleProviderNode(ctx, "backup", s.cfg.Groups.Backup, backupGroup, backupNode); ok {
				backupNode = replacementNode
				activeProbeKey = probeKey(s.cfg.Groups.Backup, backupNode)
			}
		}
		_, mainHealthy = s.ensureProvider(ctx, "main", s.cfg.Groups.Main, mainGroup)
		// Recovery is based on a fresh main provider/delay verification. The
		// public probe above checks the active backup route and is not the main
		// recovery sample consumed by the decision engine.
		currentHealthySample = true
	} else {
		return fmt.Errorf("unknown current channel %q", s.state.CurrentChannel)
	}
	s.assessPurity(ctx, activeProbeKey)

	action := s.engine.Evaluate(s.state, decision.Input{
		CurrentHealthy:         mainHealthy,
		CurrentHealthySample:   currentHealthySample,
		CurrentFailureEligible: mainFailureEligible,
		BackupHealthy:          backupHealthy,
		Now:                    s.nowUTC(),
	})
	if s.cfg.Decision.Mode == "observe" && action.Kind == decision.SwitchChannel {
		s.log("switch_observed", map[string]any{"channel": action.Channel, "reason": action.Reason})
		action.State = s.state
	}
	if action.Kind == decision.SwitchChannel && s.cfg.Decision.Mode == "auto" {
		if err := s.applyChannelSwitch(ctx, action.Channel); err != nil {
			return err
		}
		s.log("channel_switched", map[string]any{"channel": action.Channel, "reason": action.Reason})
	}
	s.state = action.State
	if err := s.store.Save(s.state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func (s *Service) assessPurity(ctx context.Context, key string) {
	if !s.cfg.Purity.Enabled {
		return
	}
	urls := append([]string(nil), s.cfg.Purity.URLs...)
	seen := make(map[string]struct{}, len(urls))
	for _, rawURL := range urls {
		seen[rawURL] = struct{}{}
	}
	for _, source := range s.cfg.Purity.Sources {
		if _, exists := seen[source.URL]; exists {
			continue
		}
		seen[source.URL] = struct{}{}
		urls = append(urls, source.URL)
	}
	if len(urls) == 0 {
		return
	}
	now := time.Now().UTC()
	if s.purity.Key == key && !s.purity.CheckedAt.IsZero() && now.Sub(s.purity.CheckedAt) < s.externalProbeInterval() {
		return
	}
	fetcher, ok := s.external.(ExternalFetcher)
	if !ok {
		s.log("purity_unknown", map[string]any{"reason": "external_fetcher_unavailable"})
		return
	}
	lookups := purity.Collect(ctx, fetcher, urls)
	result := purity.Assess(lookups)
	s.purity = cachedPurityResult{Key: key, CheckedAt: now, Result: result}
	s.log("purity_advisory", map[string]any{"score": result.Score, "warning": result.Warning, "ip": result.IP})
}

func (s *Service) activeHealthy(ctx context.Context, key string) (bool, bool, bool, bool) {
	now := s.nowUTC()
	hintSequence := s.currentProbeHintSequence()
	cacheInterval := s.externalProbeInterval()
	if !s.probe.Healthy {
		cacheInterval = s.failureRecheckInterval()
	}
	if s.probe.Key == key && !s.probe.CheckedAt.IsZero() && s.probe.HintSequence == hintSequence && now.Sub(s.probe.CheckedAt) < cacheInterval {
		return s.probe.Healthy, false, s.probe.FailureEligible, s.probe.RouteRejected
	}
	summary := s.collectCriticalProbes(ctx, false)
	s.probe = cachedProbeResult{
		Key: key, CheckedAt: now, Healthy: summary.Healthy,
		FailureEligible: summary.FailureEligible, RouteRejected: summary.RouteRejected,
		HintSequence: hintSequence,
	}
	return summary.Healthy, true, summary.FailureEligible, summary.RouteRejected
}

func (s *Service) collectCriticalProbes(ctx context.Context, requireAll bool) probeSummary {
	results := make(chan probe.Result, len(s.cfg.Probes))
	var waitGroup sync.WaitGroup
	critical := 0
	for _, item := range s.cfg.Probes {
		if !item.Enabled || !item.Critical {
			continue
		}
		critical++
		waitGroup.Add(1)
		go func(spec config.ProbeSpec) {
			defer waitGroup.Done()
			results <- s.external.Check(ctx, spec)
		}(item)
	}
	waitGroup.Wait()
	close(results)
	passed, networkFailures, routePolicyFailures := 0, 0, 0
	for result := range results {
		s.log("probe", map[string]any{"probe": result.ProbeID, "class": result.Class, "status": result.Status, "duration_ms": result.Duration.Milliseconds(), "error": result.Err})
		if result.Class == probe.ReachableHTTP {
			passed++
		}
		if result.Class == probe.NetworkError {
			networkFailures++
		}
		if result.Class == probe.RoutePolicyError {
			routePolicyFailures++
		}
	}
	// A configured vendor policy rejection is a hard incompatibility for this
	// node. It must not be hidden by a low quorum (for example OpenAI passing
	// while Gemini rejects the exit region).
	healthy := critical > 0 && routePolicyFailures == 0 && passed >= s.cfg.Decision.CriticalQuorum
	if requireAll {
		healthy = critical > 0 && routePolicyFailures == 0 && passed == critical
	}
	// An explicit vendor policy rejection is stronger than a generic HTTP
	// response: it proves this exit cannot serve that vendor.  One such
	// rejection is enough to re-qualify the node, while ordinary 5xx and
	// authentication responses remain non-attributable.
	failureEligible := critical > 0 && (routePolicyFailures > 0 || networkFailures >= s.cfg.Decision.CriticalQuorum)
	return probeSummary{Healthy: healthy, FailureEligible: failureEligible, RouteRejected: routePolicyFailures > 0}
}

func (s *Service) externalProbeInterval() time.Duration {
	if s.cfg.Decision.ProbeInterval > 0 {
		return s.cfg.Decision.ProbeInterval
	}
	return 5 * time.Minute
}

func (s *Service) failureRecheckInterval() time.Duration {
	if s.cfg.Decision.FailureRecheckInterval > 0 {
		return s.cfg.Decision.FailureRecheckInterval
	}
	return 30 * time.Second
}

func probeKey(group, node string) string {
	if node == "" {
		return group
	}
	return group + "\x00" + node
}

func (s *Service) ensureProvider(ctx context.Context, provider, groupName string, group mihomo.Proxy) (string, bool) {
	nodes := append([]string(nil), group.All...)
	if group.Now != "" {
		seen := false
		for _, node := range nodes {
			if node == group.Now {
				seen = true
				break
			}
		}
		if !seen {
			nodes = append(nodes, group.Now)
		}
	}
	if s.state.ProviderLocks == nil {
		s.state.ProviderLocks = make(map[string]state.ProviderLock)
	}
	lock := s.state.ProviderLocks[provider]
	stickyNode := lock.Node
	if stickyNode == "" {
		// On first adoption, preserve the node already carrying traffic when
		// mihomo has verified it.  This avoids an unnecessary node change just
		// because the persisted lock has not been created yet.
		stickyNode = group.Now
	}
	providerNodes, providerAvailable := s.providerNodes(ctx, provider)
	if s.providerName(provider) != "" && !providerAvailable {
		s.requestProviderHealthCheck(ctx, provider)
		return "", false
	}
	candidates := make([]decision.Candidate, 0, len(nodes))
	for order, node := range nodes {
		healthy, delay := s.nodeHealthy(ctx, node, providerNodes)
		if healthy {
			candidates = append(candidates, decision.Candidate{Name: node, Healthy: true, Score: -delay, Order: order})
		}
	}
	chosen := ""
	if qualityNode, ok := s.qualityReplacement(ctx, provider, groupName, stickyNode, providerNodes, nodes); ok {
		chosen = qualityNode
	}
	if chosen == "" {
		chosen = decision.ChooseNode(stickyNode, candidates)
	}
	if chosen == "" {
		s.requestProviderHealthCheck(ctx, provider)
		s.log("provider_unverified", map[string]any{"provider": provider, "group": groupName})
		return "", false
	}
	if group.Now != chosen {
		if err := s.api.SetProxy(ctx, groupName, chosen); err != nil {
			s.log("node_select_failed", map[string]any{"provider": provider, "group": groupName, "node": chosen, "error": err.Error()})
			return "", false
		}
	}
	s.state.ProviderLocks[provider] = state.ProviderLock{Provider: provider, Group: groupName, Node: chosen, LastVerifiedAt: s.nowUTC()}
	s.log("node_verified", map[string]any{"provider": provider, "group": groupName, "node": chosen})
	return chosen, true
}

// findCompatibleProviderNode is used only after an explicit route-policy
// rejection (for example Gemini's "user location is not supported").  It
// tests the other mihomo-verified nodes through the normal loopback proxy and
// requires every enabled critical vendor probe to pass.  The selected group
// may be the live group, but the channel selector is never touched here; if no
// candidate qualifies, the original node is restored and the caller keeps its
// existing failover decision.
func (s *Service) findCompatibleProviderNode(ctx context.Context, provider, groupName string, group mihomo.Proxy, currentNode string) (string, probeSummary, bool) {
	providerNodes, providerAvailable := s.providerNodes(ctx, provider)
	if s.providerName(provider) != "" && !providerAvailable {
		return "", probeSummary{}, false
	}

	for _, node := range group.All {
		if node == "" || node == currentNode {
			continue
		}
		if providerNodes != nil {
			metadata, exists := providerNodes[node]
			if !exists || !metadata.Alive || len(metadata.History) == 0 {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			s.restoreProviderNode(ctx, provider, groupName, currentNode)
			return "", probeSummary{}, false
		}
		if err := s.api.SetProxy(ctx, groupName, node); err != nil {
			s.log("candidate_node_select_failed", map[string]any{"provider": provider, "group": groupName, "error": err.Error()})
			continue
		}
		s.log("candidate_node_probe_started", map[string]any{"provider": provider, "group": groupName})
		summary := s.collectCriticalProbes(ctx, true)
		if summary.Healthy {
			s.state.ProviderLocks[provider] = state.ProviderLock{
				Provider: provider, Group: groupName, Node: node, LastVerifiedAt: s.nowUTC(),
			}
			s.log("candidate_node_verified", map[string]any{"provider": provider, "group": groupName})
			return node, summary, true
		}
		s.log("candidate_node_rejected", map[string]any{"provider": provider, "group": groupName, "route_policy_rejected": summary.RouteRejected})
	}

	if currentNode != "" {
		s.restoreProviderNode(ctx, provider, groupName, currentNode)
	}
	s.log("compatible_node_not_found", map[string]any{"provider": provider, "group": groupName})
	return "", probeSummary{}, false
}

func (s *Service) restoreProviderNode(ctx context.Context, provider, groupName, node string) {
	if node == "" {
		return
	}
	// Candidate qualification can be canceled after it has already changed the
	// generated group. Remove cancellation from the compensating write so the
	// live group is never left on an unqualified node.
	restoreCtx := context.WithoutCancel(ctx)
	if err := s.api.SetProxy(restoreCtx, groupName, node); err != nil {
		s.log("candidate_node_restore_failed", map[string]any{"provider": provider, "group": groupName, "error": err.Error()})
	}
}

type mihomoHeartbeater interface {
	Heartbeat(context.Context) error
}

func (s *Service) refreshRecommendations(ctx context.Context) error {
	if s.quality == nil || !s.cfg.Quality.Enabled {
		s.recs = nil
		return nil
	}
	heartbeater, ok := s.api.(mihomoHeartbeater)
	if !ok {
		s.recs = nil
		s.log("quality_recommendations_ignored", map[string]any{"reason": "runtime_api_does_not_expose_heartbeat"})
		return nil
	}
	if err := heartbeater.Heartbeat(ctx); err != nil {
		s.recs = nil
		s.log("quality_recommendations_ignored", map[string]any{"reason": "quality_link_unavailable", "error": err.Error()})
		return nil
	}
	maxAge := s.cfg.Quality.FullScanInterval
	if maxAge <= 0 {
		maxAge = 720 * time.Hour
	}
	recommendations, err := quality.ReadRecommendations(s.quality, time.Now().UTC(), maxAge)
	if err != nil {
		// A recommendation is advisory state. If its file is unreadable, keep
		// the already loaded sticky/provider behavior and do not interrupt the
		// production loop.
		s.recs = nil
		s.log("quality_recommendations_ignored", map[string]any{"reason": err.Error()})
		return nil
	}
	s.recs = recommendations
	return nil
}

func (s *Service) qualityTarget(provider, groupName string) (config.QualityTarget, bool) {
	if !s.cfg.Quality.Enabled || (groupName != s.cfg.Groups.Main && groupName != s.cfg.Groups.Backup) {
		return config.QualityTarget{}, false
	}
	expectedProvider := s.providerName(provider)
	for _, target := range s.cfg.Quality.Targets {
		if target.SourceGroup != groupName {
			continue
		}
		if expectedProvider != "" && target.Provider != expectedProvider {
			continue
		}
		return target, true
	}
	return config.QualityTarget{}, false
}

func (s *Service) qualityReplacement(ctx context.Context, provider, groupName, stickyNode string, providerNodes map[string]mihomo.Proxy, sourceNodes []string) (string, bool) {
	if len(s.recs) == 0 || providerNodes == nil {
		return "", false
	}
	target, ok := s.qualityTarget(provider, groupName)
	if !ok {
		return "", false
	}
	now := s.nowUTC()
	maxAge := s.cfg.Quality.FullScanInterval
	if maxAge <= 0 {
		maxAge = 720 * time.Hour
	}
	thresholds := s.cfg.Quality.Thresholds
	if thresholds.BaselineDropPoints <= 0 {
		thresholds.BaselineDropPoints = 20
	}
	if thresholds.MinimumConfidence <= 0 {
		thresholds.MinimumConfidence = 70
	}
	if thresholds.CandidateMinimumScore <= 0 {
		thresholds.CandidateMinimumScore = 60
	}

	allowed := make(map[string]struct{}, len(sourceNodes))
	for _, node := range sourceNodes {
		allowed[node] = struct{}{}
	}
	var candidates []quality.Recommendation
	var sticky *quality.Recommendation
	for _, recommendation := range s.recs {
		if recommendation.Target != target.ID || recommendation.SourceGroup != groupName || recommendation.Provider != target.Provider {
			continue
		}
		if _, exists := allowed[recommendation.Node]; !exists {
			continue
		}
		metadata, exists := providerNodes[recommendation.Node]
		if !exists {
			continue
		}
		validation := quality.RecommendationValidation{
			Target: target, CurrentNode: recommendation.Node, CurrentIP: recommendation.Identity.IP,
			CurrentProvider: target.Provider, ProviderAlive: metadata.Alive,
			ProviderHistoryFresh:    s.qualityHistoryFresh(metadata.History, now),
			VendorConnectivityFresh: recommendation.Connected,
			Now:                     now, MaxAge: maxAge, MinimumScore: thresholds.CandidateMinimumScore,
			MinimumConfidence: thresholds.MinimumConfidence,
		}
		if recommendation.Node == stickyNode {
			if err := quality.ValidateStickyRecommendation(recommendation, validation, thresholds.MinimumConfidence); err != nil {
				s.log("quality_recommendation_rejected", map[string]any{"target": target.ID, "node": recommendation.Node, "reason": err.Error()})
				continue
			}
			if sticky == nil || recommendation.ReportedAt.After(sticky.ReportedAt) {
				value := recommendation
				sticky = &value
			}
			continue
		}
		if err := quality.ValidateRecommendation(recommendation, validation); err != nil {
			s.log("quality_recommendation_rejected", map[string]any{"target": target.ID, "node": recommendation.Node, "reason": err.Error()})
			continue
		}
		candidates = append(candidates, recommendation)
	}
	if len(candidates) == 0 || sticky == nil {
		return "", false
	}
	// ReadRecommendations is deterministic, but sort again after filtering so
	// this method remains deterministic if callers inject recommendations.
	sortQualityRecommendations(candidates)
	for index := range candidates {
		candidate := candidates[index]
		if candidate.Node == sticky.Node {
			continue
		}
		stickyMetadata, exists := providerNodes[sticky.Node]
		if !exists {
			continue
		}
		request := quality.ReplacementRequest{
			Sticky: *sticky, Candidate: candidate,
			StickyValidation: quality.RecommendationValidation{
				Target: target, CurrentNode: sticky.Node, CurrentIP: sticky.Identity.IP,
				CurrentProvider: target.Provider, ProviderAlive: stickyMetadata.Alive,
				ProviderHistoryFresh:    s.qualityHistoryFresh(stickyMetadata.History, now),
				VendorConnectivityFresh: sticky.Connected,
				Now:                     now, MaxAge: maxAge, MinimumConfidence: thresholds.MinimumConfidence,
			},
			CandidateValidation: quality.RecommendationValidation{
				Target: target, CurrentNode: candidate.Node, CurrentIP: candidate.Identity.IP,
				CurrentProvider: target.Provider, ProviderAlive: true,
				ProviderHistoryFresh: true, VendorConnectivityFresh: true,
				Now: now, MaxAge: maxAge, MinimumScore: thresholds.CandidateMinimumScore,
				MinimumConfidence: thresholds.MinimumConfidence,
			},
			Thresholds: thresholds,
		}
		if replace, reason := quality.EvaluateReplacement(request); replace {
			s.log("quality_recommendation_accepted", map[string]any{"target": target.ID, "node": candidate.Node, "reason": reason})
			return candidate.Node, true
		} else {
			s.log("quality_recommendation_rejected", map[string]any{"target": target.ID, "node": candidate.Node, "reason": reason})
		}
	}
	return "", false
}

func sortQualityRecommendations(items []quality.Recommendation) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].EffectiveScore != items[j].EffectiveScore {
			return items[i].EffectiveScore > items[j].EffectiveScore
		}
		if !items[i].ReportedAt.Equal(items[j].ReportedAt) {
			return items[i].ReportedAt.After(items[j].ReportedAt)
		}
		return items[i].Node < items[j].Node
	})
}

func (s *Service) qualityHistoryFresh(history []mihomo.DelayHistory, now time.Time) bool {
	if len(history) == 0 {
		return false
	}
	latest := history[0]
	for _, item := range history[1:] {
		if item.Time.After(latest.Time) {
			latest = item
		}
	}
	if latest.Time.IsZero() || latest.Time.After(now.Add(2*time.Minute)) {
		return false
	}
	staleAfter := s.cfg.Quality.Stability.StaleAfter
	if staleAfter <= 0 {
		staleAfter = 26 * time.Hour
	}
	return !latest.Time.Before(now.Add(-staleAfter))
}

func (s *Service) providerName(provider string) string {
	if provider == "main" {
		return s.cfg.Providers.Main
	}
	if provider == "backup" {
		return s.cfg.Providers.Backup
	}
	return ""
}

func (s *Service) providerNodes(ctx context.Context, provider string) (map[string]mihomo.Proxy, bool) {
	providerName := s.providerName(provider)
	if providerName == "" {
		return nil, true
	}
	providerAPI, ok := s.api.(ProviderAPI)
	if !ok {
		s.log("provider_health_unavailable", map[string]any{"provider": provider, "name": providerName, "reason": "api_does_not_support_provider_metadata"})
		return nil, false
	}
	metadata, err := providerAPI.GetProvider(ctx, providerName)
	if err != nil {
		s.log("provider_health_unavailable", map[string]any{"provider": provider, "name": providerName, "error": err.Error()})
		return nil, false
	}
	result := make(map[string]mihomo.Proxy, len(metadata.Proxies))
	for _, node := range metadata.Proxies {
		if node.Name != "" {
			result[node.Name] = node
		}
	}
	return result, true
}

func (s *Service) requestProviderHealthCheck(ctx context.Context, provider string) {
	providerName := s.providerName(provider)
	if providerName == "" {
		return
	}
	checker, ok := s.api.(ProviderHealthChecker)
	if !ok {
		return
	}
	if s.providerHealthChecks == nil {
		s.providerHealthChecks = make(map[string]time.Time)
	}
	now := s.nowUTC()
	interval := s.cfg.Decision.RecoveryHealthcheckInterval
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	if last, exists := s.providerHealthChecks[provider]; exists && now.Sub(last) < interval {
		return
	}
	// Mark before the request so an API error cannot turn a 15-second guardian
	// loop into a provider health-check storm. The next interval retries it.
	s.providerHealthChecks[provider] = now
	if err := checker.HealthCheckProvider(ctx, providerName); err != nil {
		s.log("provider_healthcheck_failed", map[string]any{"provider": provider, "name": providerName, "error": err.Error()})
		return
	}
	s.log("provider_healthcheck_requested", map[string]any{"provider": provider, "name": providerName})
}

func (s *Service) nodeHealthy(ctx context.Context, node string, providerNodes map[string]mihomo.Proxy) (bool, int) {
	if providerNodes != nil {
		candidate, ok := providerNodes[node]
		if !ok || !candidate.Alive {
			return false, 0
		}
		if delay, ok := latestDelay(candidate.History); ok {
			return true, delay
		}
		return false, 0
	}
	passed, total, delaySum := 0, 0, 0
	for _, item := range s.cfg.Probes {
		if !item.Enabled || !item.Critical {
			continue
		}
		total++
		delay, err := s.api.Delay(ctx, node, item.URL, item.DelayTimeout)
		if err == nil {
			passed++
			delaySum += delay
		}
	}
	return total > 0 && passed >= s.cfg.Decision.CriticalQuorum, delaySum
}

func latestDelay(history []mihomo.DelayHistory) (int, bool) {
	if len(history) == 0 {
		return 0, false
	}
	latest := history[0]
	for _, item := range history[1:] {
		if item.Time.After(latest.Time) {
			latest = item
		}
	}
	return latest.Delay, true
}

func (s *Service) applyChannelSwitch(ctx context.Context, channel string) error {
	provider := "main"
	groupName := s.cfg.Groups.Main
	if channel == s.cfg.Groups.Backup {
		provider, groupName = "backup", s.cfg.Groups.Backup
	}
	lock := s.state.ProviderLocks[provider]
	if lock.Node == "" {
		return fmt.Errorf("verified node missing for channel %q", channel)
	}
	target, err := s.api.GetProxy(ctx, groupName)
	if err != nil {
		return fmt.Errorf("read %s before switch: %w", groupName, err)
	}
	if target.Now != lock.Node {
		if err := s.api.SetProxy(ctx, groupName, lock.Node); err != nil {
			return fmt.Errorf("select %s node %q: %w", groupName, lock.Node, err)
		}
	}
	if err := s.api.SetProxy(ctx, s.cfg.Groups.Channel, channel); err != nil {
		return fmt.Errorf("switch channel to %q: %w", channel, err)
	}
	return nil
}

func (s *Service) log(event string, fields map[string]any) {
	if s.logger != nil {
		_ = s.logger.Event(event, fields)
	}
}
