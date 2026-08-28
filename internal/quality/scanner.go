package quality

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/state"
)

var (
	// ErrQualityLink means the scanner could not establish its control-plane
	// connection to mihomo. Callers can use errors.Is to distinguish this
	// fail-closed condition from an individual node failure.
	ErrQualityLink = errors.New("quality-link: mihomo unavailable")
	ErrScanner     = errors.New("quality scanner error")
)

// MihomoAPI is the deliberately small control-plane surface used by the
// scanner. In particular, SetProxy is only ever called with a generated
// GUARDIAN-QUALITY-* group; source groups and CHANNEL remain read-only here.
type MihomoAPI interface {
	Heartbeat(context.Context) error
	GetProxy(context.Context, string) (mihomo.Proxy, error)
	GetProvider(context.Context, string) (mihomo.Provider, error)
	SetProxy(context.Context, string, string) error
}

// Logger is compatible with logging.Logger while keeping this package free
// from a dependency on a concrete logger implementation. Logging failures do
// not interrupt a quality scan.
type Logger interface {
	Event(string, map[string]any) error
}

// CollectFunc is an optional seam for deterministic tests and future
// collectors. The production default constructs Collector with the concrete
// *probe.ExternalClient returned by External, so public requests still use the
// target's loopback mihomo listener.
type CollectFunc func(context.Context, *probe.ExternalClient, []SourceSpec, []VendorProbeSpec) (Collection, error)

type Scanner struct {
	API      MihomoAPI
	Reports  *Store
	State    *state.Store
	Logger   Logger
	External func(string, time.Duration) (*probe.ExternalClient, error)

	// Collect is intentionally optional. It exists to make the scanner's
	// ordering and persistence behavior testable without making real public
	// requests. When nil, the existing Collector is used.
	Collect CollectFunc

	// Now is an optional clock seam. It is not required by callers and is kept
	// out of the public configuration so scan timestamps remain operational
	// data rather than behavior configuration.
	Now func() time.Time
}

type scanCandidate struct {
	name         string
	providerNode mihomo.Proxy
	hasProvider  bool
}

// Scan heartbeats once before doing any target work, then processes targets
// exactly in quality.order. A node failure is recorded and does not prevent
// the following node or target from being attempted. A broken mihomo link is
// different: it is returned immediately and no selection or external client
// is created.
func (s *Scanner) Scan(ctx context.Context, cfg config.Config) error {
	if err := s.heartbeat(ctx); err != nil {
		return err
	}
	if !cfg.Quality.Enabled {
		return nil
	}

	byID := make(map[string]config.QualityTarget, len(cfg.Quality.Targets))
	for _, target := range cfg.Quality.Targets {
		byID[target.ID] = target
	}
	var firstErr error
	for _, id := range cfg.Quality.Order {
		target, ok := byID[id]
		if !ok {
			err := fmt.Errorf("%w: target %q is not configured", ErrScanner, id)
			s.log("quality_target_failed", map[string]any{"target": id, "error": err.Error()})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.scanTarget(ctx, cfg, target); err != nil {
			if errors.Is(err, ErrQualityLink) {
				return err
			}
			s.log("quality_target_failed", map[string]any{"target": target.ID, "error": err.Error()})
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ScanTarget is the direct single-target entry point. It independently
// heartbeats first so a caller cannot accidentally select a quality node or
// construct a public client while mihomo is disconnected.
func (s *Scanner) ScanTarget(ctx context.Context, cfg config.Config, target config.QualityTarget) error {
	if err := s.heartbeat(ctx); err != nil {
		return err
	}
	return s.scanTarget(ctx, cfg, target)
}

func (s *Scanner) heartbeat(ctx context.Context) error {
	if s == nil || s.API == nil {
		return fmt.Errorf("%w: API dependency is missing", ErrQualityLink)
	}
	if err := s.API.Heartbeat(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrQualityLink, err)
	}
	return nil
}

func (s *Scanner) scanTarget(ctx context.Context, cfg config.Config, target config.QualityTarget) error {
	if s.Reports == nil {
		return fmt.Errorf("%w: report store is missing", ErrScanner)
	}
	if strings.TrimSpace(target.ID) == "" {
		return fmt.Errorf("%w: quality target id is empty", ErrScanner)
	}

	candidates, err := s.resolveCandidates(ctx, target)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, candidate.name)
	}
	fingerprint := scannerCandidateFingerprint(names)

	progress, err := s.Reports.LoadScanProgress()
	if err != nil {
		return fmt.Errorf("%w: load scan progress: %v", ErrScanner, err)
	}
	if progress.Targets == nil {
		progress.Targets = make(map[string]TargetScanProgress)
	}
	targetProgress := progress.Targets[target.ID]
	if targetProgress.Target == target.ID &&
		targetProgress.Provider == target.Provider &&
		targetProgress.ProviderFingerprint == fingerprint &&
		targetProgress.Complete {
		return nil
	}
	if targetProgress.Target != target.ID || targetProgress.Provider != target.Provider || targetProgress.ProviderFingerprint != fingerprint {
		targetProgress = TargetScanProgress{Target: target.ID, Provider: target.Provider, ProviderFingerprint: fingerprint}
	}
	start := resumeIndex(targetProgress, names)
	if start < 0 {
		start = 0
	}
	if start > len(candidates) {
		start = len(candidates)
	}

	for index := start; index < len(candidates); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate := candidates[index]
		now := s.now()
		targetProgress.Target = target.ID
		targetProgress.Provider = target.Provider
		targetProgress.ProviderFingerprint = fingerprint
		targetProgress.Cursor = candidate.name
		targetProgress.CursorIndex = index
		targetProgress.LastAttemptAt = now
		targetProgress.Complete = false
		if err := s.saveTargetProgress(&progress, targetProgress); err != nil {
			return fmt.Errorf("%w: save scan progress before node %q: %v", ErrScanner, candidate.name, err)
		}

		nodeCtx, cancel := context.WithTimeout(ctx, s.perNodeTimeout(cfg))
		success, nodeErr := s.scanNode(nodeCtx, cfg, target, candidate)
		cancel()
		if nodeErr != nil {
			s.log("quality_node_failed", map[string]any{
				"target": target.ID, "provider": target.Provider, "node": candidate.name, "error": nodeErr.Error(),
			})
		}
		targetProgress.Attempted++
		targetProgress.Cursor = candidate.name
		targetProgress.CursorIndex = index + 1
		targetProgress.LastAttemptAt = s.now()
		if success {
			targetProgress.Completed++
			targetProgress.LastSuccessAt = targetProgress.LastAttemptAt
		}
		targetProgress.Complete = index+1 >= len(candidates)
		if err := s.saveTargetProgress(&progress, targetProgress); err != nil {
			return fmt.Errorf("%w: save scan progress after node %q: %v", ErrScanner, candidate.name, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	targetProgress.CursorIndex = len(candidates)
	targetProgress.Complete = true
	if err := s.saveTargetProgress(&progress, targetProgress); err != nil {
		return fmt.Errorf("%w: save completed scan progress for target %q: %v", ErrScanner, target.ID, err)
	}
	s.log("quality_target_complete", map[string]any{
		"target": target.ID, "provider": target.Provider, "nodes": len(candidates), "completed": targetProgress.Completed,
	})
	return nil
}

func (s *Scanner) resolveCandidates(ctx context.Context, target config.QualityTarget) ([]scanCandidate, error) {
	if s.API == nil {
		return nil, fmt.Errorf("%w: API dependency is missing", ErrScanner)
	}
	sourceGroup := target.SourceGroup
	if strings.TrimSpace(sourceGroup) == "" {
		return nil, fmt.Errorf("%w: target %q source group is empty", ErrScanner, target.ID)
	}
	group, err := s.API.GetProxy(ctx, sourceGroup)
	if err != nil {
		return nil, fmt.Errorf("%w: read source group %q: %v", ErrScanner, sourceGroup, err)
	}

	providerNodes := make(map[string]mihomo.Proxy)
	providerConfigured := strings.TrimSpace(target.Provider) != ""
	if providerConfigured {
		providerName := target.Provider
		provider, err := s.API.GetProvider(ctx, providerName)
		if err != nil {
			return nil, fmt.Errorf("%w: read provider %q: %v", ErrScanner, providerName, err)
		}
		for _, node := range provider.Proxies {
			name := strings.TrimSpace(node.Name)
			if name == "" {
				continue
			}
			if _, exists := providerNodes[name]; !exists {
				providerNodes[name] = node
			}
		}
	}

	filter, err := compileNodeFilter(target.NodeFilter)
	if err != nil {
		return nil, fmt.Errorf("%w: target %q node_filter: %v", ErrScanner, target.ID, err)
	}

	// mihomo's source group order is authoritative for the intersection. It
	// is the order users see and select in that group; provider metadata only
	// supplies membership and health/history, never extra nodes.
	sourceNodes := orderedUnique(group.All)
	if target.Scope == "locked" {
		if s.State == nil {
			return nil, fmt.Errorf("%w: target %q requires guardian state for locked scope", ErrScanner, target.ID)
		}
		stored, err := s.State.Load()
		if err != nil {
			return nil, fmt.Errorf("%w: load guardian state for target %q: %v", ErrScanner, target.ID, err)
		}
		lock, ok := stored.ProviderLocks[target.LockKey]
		if !ok || strings.TrimSpace(lock.Node) == "" {
			return nil, fmt.Errorf("%w: target %q lock %q is not available", ErrScanner, target.ID, target.LockKey)
		}
		for _, node := range sourceNodes {
			if node != lock.Node {
				continue
			}
			if providerConfigured {
				providerNode, exists := providerNodes[node]
				if !exists {
					return nil, fmt.Errorf("%w: locked node %q is not in provider %q", ErrScanner, node, target.Provider)
				}
				return []scanCandidate{{name: node, providerNode: providerNode, hasProvider: true}}, nil
			}
			return []scanCandidate{{name: node}}, nil
		}
		return nil, fmt.Errorf("%w: locked node %q is not in source group %q", ErrScanner, lock.Node, sourceGroup)
	}
	if target.Scope != "all" {
		return nil, fmt.Errorf("%w: target %q scope must be locked or all", ErrScanner, target.ID)
	}

	candidates := make([]scanCandidate, 0, len(sourceNodes))
	for _, node := range sourceNodes {
		if filter != nil && !filter.MatchString(node) {
			continue
		}
		if providerConfigured {
			providerNode, exists := providerNodes[node]
			if !exists {
				continue
			}
			candidates = append(candidates, scanCandidate{name: node, providerNode: providerNode, hasProvider: true})
			continue
		}
		candidates = append(candidates, scanCandidate{name: node})
	}
	return candidates, nil
}

func compileNodeFilter(raw string) (*regexp.Regexp, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	return regexp.Compile(raw)
}

func orderedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func scannerCandidateFingerprint(names []string) string {
	hash := sha256.New()
	for _, name := range names {
		name = strings.TrimSpace(name)
		_, _ = hash.Write([]byte(fmt.Sprintf("%d:", len(name))))
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func resumeIndex(progress TargetScanProgress, names []string) int {
	if progress.CursorIndex > 0 && progress.CursorIndex <= len(names) {
		if progress.Cursor == "" || names[progress.CursorIndex-1] == progress.Cursor {
			return progress.CursorIndex
		}
	}
	if progress.Cursor != "" {
		for index, name := range names {
			if name == progress.Cursor {
				return index + 1
			}
		}
	}
	return 0
}

func (s *Scanner) scanNode(ctx context.Context, cfg config.Config, target config.QualityTarget, candidate scanCandidate) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	qualityGroup := "GUARDIAN-QUALITY-" + target.ID
	if err := s.API.SetProxy(ctx, qualityGroup, candidate.name); err != nil {
		return false, fmt.Errorf("select quality group %q node %q: %w", qualityGroup, candidate.name, err)
	}

	nodeProxy := candidate.providerNode
	if !candidate.hasProvider {
		var err error
		nodeProxy, err = s.API.GetProxy(ctx, candidate.name)
		if err != nil {
			return false, fmt.Errorf("read node %q: %w", candidate.name, err)
		}
	}

	factory := s.External
	if factory == nil {
		factory = probe.NewExternalClient
	}
	client, err := factory(target.Listener, s.perNodeTimeout(cfg))
	if err != nil {
		return false, fmt.Errorf("create external client for target listener: %w", err)
	}

	sources := scannerSources(cfg)
	vendors := scannerVendors(cfg)
	collection, err := s.collect(ctx, client, sources, vendors)
	if err != nil {
		return false, fmt.Errorf("collect node %q evidence: %w", candidate.name, err)
	}

	now := s.now()
	identityIP, identityFamily, identityComplete := collectionIdentity(collection)
	if !identityComplete {
		return false, errors.New("collection did not produce a two-source IP identity")
	}
	providerIdentity := firstNonEmpty(target.Provider, nodeProxy.ProviderName)
	providerIdentity = firstNonEmpty(providerIdentity, target.SourceGroup)
	key := NodeKey{Target: target.ID, Provider: providerIdentity, Node: candidate.name, IPFamily: identityFamily, IP: identityIP}.Canonical()
	if err := key.Validate(); err != nil {
		return false, fmt.Errorf("invalid collected identity for node %q: %w", candidate.name, err)
	}

	stability := AggregateStability([]mihomo.Proxy{nodeProxy}, candidate.name, now, cfg.Quality.Stability)
	stability.Identity = key
	if err := s.Reports.SaveStability(stability); err != nil {
		return false, fmt.Errorf("save stability for node %q: %w", candidate.name, err)
	}

	report := Report{
		Identity:       key,
		ObservedAt:     now,
		VendorResults:  collection.VendorResults,
		SourceEvidence: collection.SourceEvidence,
		RiskEvidence:   collection.RiskEvidence,
		Provider: ProviderHealth{
			Alive:          nodeProxy.Alive,
			HistorySamples: stability.Samples,
			LastSampleAt:   stability.LastSampleAt,
			CheckedAt:      now,
		},
		ProviderAlive:          nodeProxy.Alive,
		ProviderHistorySamples: stability.Samples,
		ProviderLastSampleAt:   stability.LastSampleAt,
		StabilityScore:         stability.StabilityScore,
		Complete:               collection.IdentityComplete,
		Errors:                 append([]ReportError(nil), collection.Errors...),
	}
	report = ScoreReport(report)
	historyPresent := len(nodeProxy.History) > 0
	report.ProviderHistoryFresh = historyPresent && stability.Fresh
	report.Provider.HistoryFresh = report.ProviderHistoryFresh
	report.ProviderHistorySamples = stability.Samples
	report.Provider.HistorySamples = stability.Samples
	report.ProviderLastSampleAt = stability.LastSampleAt
	report.Provider.LastSampleAt = stability.LastSampleAt
	report.Complete = report.Complete && nodeProxy.Alive && historyPresent && stability.Known && stability.Fresh
	report.Eligible = report.Complete && report.ConfidencePercent >= minimumConfidence(cfg.Quality)
	if !report.ProviderAlive {
		report.Errors = append(report.Errors, ReportError{Code: ErrorProviderUnhealthy, Source: target.Provider, Message: "mihomo provider node is not alive", ObservedAt: now})
	}
	if !report.ProviderHistoryFresh {
		report.Errors = append(report.Errors, ReportError{Code: ErrorStabilityUnknown, Source: target.Provider, Message: "mihomo history is missing or stale", ObservedAt: now})
	}
	if _, err := s.Reports.SaveReport(report); err != nil {
		return false, fmt.Errorf("save report for node %q: %w", candidate.name, err)
	}
	s.log("quality_node_complete", map[string]any{
		"target": target.ID, "provider": target.Provider, "node": candidate.name,
		"effective_score": report.EffectiveScore, "eligible": report.Eligible,
	})
	return true, nil
}

func (s *Scanner) collect(ctx context.Context, client *probe.ExternalClient, sources []SourceSpec, vendors []VendorProbeSpec) (Collection, error) {
	if s.Collect != nil {
		return s.Collect(ctx, client, sources, vendors)
	}
	return (&Collector{Client: client, Sources: sources, Vendors: vendors}).Collect(ctx)
}

func collectionIdentity(collection Collection) (string, string, bool) {
	ip := strings.TrimSpace(collection.IdentityIP)
	family := strings.TrimSpace(collection.IdentityFamily)
	complete := collection.IdentityComplete
	if ip == "" || net.ParseIP(ip) == nil {
		var derivedComplete bool
		ip, family, derivedComplete, _ = consensusIdentity(collection.SourceEvidence)
		complete = complete || derivedComplete
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", "", false
	}
	ip = parsed.String()
	if family == "" {
		family = ipFamily(ip)
	}
	return ip, family, complete
}

func scannerSources(cfg config.Config) []SourceSpec {
	if len(cfg.Purity.URLs) == 0 {
		return nil
	}
	result := make([]SourceSpec, 0, len(cfg.Purity.URLs))
	for index, raw := range cfg.Purity.URLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		format := SourceFormatText
		kind := SourceKindIP
		if parsed, err := url.Parse(raw); err == nil {
			path := strings.ToLower(parsed.Path + "?" + parsed.RawQuery)
			if strings.Contains(path, "json") {
				format = SourceFormatJSON
				kind = SourceKindIdentity
			}
		}
		result = append(result, SourceSpec{
			ID: fmt.Sprintf("purity-%d", index+1), URL: raw, Kind: kind, Format: format,
			Timeout:  cfg.Purity.Timeout,
			Critical: true,
		})
	}
	return result
}

func scannerVendors(cfg config.Config) []VendorProbeSpec {
	enabled := make([]config.ProbeSpec, 0, len(cfg.Probes))
	for _, item := range cfg.Probes {
		if item.Enabled {
			enabled = append(enabled, item)
		}
	}
	return VendorProbesFromConfig(enabled)
}

func (s *Scanner) saveTargetProgress(progress *ScanProgress, target TargetScanProgress) error {
	if progress.Targets == nil {
		progress.Targets = make(map[string]TargetScanProgress)
	}
	progress.Targets[target.Target] = target
	progress.Target = target.Target
	progress.Provider = target.Provider
	progress.Cursor = target.Cursor
	progress.CursorIndex = target.CursorIndex
	progress.ProviderFingerprint = target.ProviderFingerprint
	progress.Attempted = target.Attempted
	progress.Completed = target.Completed
	progress.LastAttemptAt = target.LastAttemptAt
	progress.LastSuccessAt = target.LastSuccessAt
	progress.Complete = target.Complete
	progress.UpdatedAt = s.now()
	return s.Reports.SaveScanProgress(*progress)
}

func (s *Scanner) perNodeTimeout(cfg config.Config) time.Duration {
	if cfg.Quality.PerNodeTimeout > 0 {
		return cfg.Quality.PerNodeTimeout
	}
	return 180 * time.Second
}

func minimumConfidence(quality config.QualityConfig) int {
	if quality.Thresholds.MinimumConfidence <= 0 {
		return 70
	}
	return quality.Thresholds.MinimumConfidence
}

func (s *Scanner) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scanner) log(event string, fields map[string]any) {
	if s == nil || s.Logger == nil {
		return
	}
	_ = s.Logger.Event(event, fields)
}
