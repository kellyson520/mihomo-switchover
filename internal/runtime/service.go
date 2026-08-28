package runtime

import (
	"context"
	"fmt"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/decision"
	"mihomo-guardian/internal/logging"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/probe"
	"mihomo-guardian/internal/purity"
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

type ExternalProbe interface {
	Check(context.Context, config.ProbeSpec) probe.Result
}

type ExternalFetcher interface {
	Fetch(context.Context, config.ProbeSpec) (probe.Result, []byte)
}

type Service struct {
	cfg      config.Config
	api      API
	external ExternalProbe
	store    *state.Store
	logger   *logging.Logger
	state    state.State
	loaded   bool
	engine   *decision.Engine
}

func NewService(cfg config.Config, api API, external ExternalProbe, store *state.Store, logger *logging.Logger) *Service {
	return &Service{
		cfg: cfg, api: api, external: external, store: store, logger: logger,
		engine: newDecisionEngine(cfg),
	}
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
}

func (s *Service) RunCycle(ctx context.Context) error {
	loaded, err := s.store.Load()
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	s.state, s.loaded = loaded, true

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
	if s.state.CurrentChannel == s.cfg.Groups.Main {
		_, _ = s.ensureProvider(ctx, "main", s.cfg.Groups.Main, mainGroup)
		activeHealthy := s.activeHealthy(ctx)
		mainHealthy = activeHealthy
		if !activeHealthy {
			_, backupHealthy = s.ensureProvider(ctx, "backup", s.cfg.Groups.Backup, backupGroup)
		}
	} else if s.state.CurrentChannel == s.cfg.Groups.Backup {
		_, _ = s.ensureProvider(ctx, "backup", s.cfg.Groups.Backup, backupGroup)
		backupHealthy = s.activeHealthy(ctx)
		_, mainHealthy = s.ensureProvider(ctx, "main", s.cfg.Groups.Main, mainGroup)
	} else {
		return fmt.Errorf("unknown current channel %q", s.state.CurrentChannel)
	}
	s.assessPurity(ctx)

	action := s.engine.Evaluate(s.state, decision.Input{
		CurrentHealthy: mainHealthy,
		BackupHealthy:  backupHealthy,
		Now:            time.Now(),
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

func (s *Service) assessPurity(ctx context.Context) {
	if !s.cfg.Purity.Enabled || len(s.cfg.Purity.URLs) == 0 {
		return
	}
	fetcher, ok := s.external.(ExternalFetcher)
	if !ok {
		s.log("purity_unknown", map[string]any{"reason": "external_fetcher_unavailable"})
		return
	}
	lookups := purity.Collect(ctx, fetcher, s.cfg.Purity.URLs)
	result := purity.Assess(lookups)
	s.log("purity_advisory", map[string]any{"score": result.Score, "warning": result.Warning, "ip": result.IP})
}

func (s *Service) activeHealthy(ctx context.Context) bool {
	passed := 0
	critical := 0
	for _, item := range s.cfg.Probes {
		if !item.Enabled || !item.Critical {
			continue
		}
		critical++
		result := s.external.Check(ctx, item)
		s.log("probe", map[string]any{"probe": item.ID, "class": result.Class, "status": result.Status, "duration_ms": result.Duration.Milliseconds(), "error": result.Err})
		if result.Class == probe.ReachableHTTP {
			passed++
		}
	}
	return critical > 0 && passed >= s.cfg.Decision.CriticalQuorum
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
		return "", false
	}
	candidates := make([]decision.Candidate, 0, len(nodes))
	for order, node := range nodes {
		healthy, delay := s.nodeHealthy(ctx, node, providerNodes)
		if healthy {
			candidates = append(candidates, decision.Candidate{Name: node, Healthy: true, Score: -delay, Order: order})
		}
	}
	chosen := decision.ChooseNode(stickyNode, candidates)
	if chosen == "" {
		s.log("provider_unverified", map[string]any{"provider": provider, "group": groupName})
		return "", false
	}
	if group.Now != chosen {
		if err := s.api.SetProxy(ctx, groupName, chosen); err != nil {
			s.log("node_select_failed", map[string]any{"provider": provider, "group": groupName, "node": chosen, "error": err.Error()})
			return "", false
		}
	}
	s.state.ProviderLocks[provider] = state.ProviderLock{Provider: provider, Group: groupName, Node: chosen, LastVerifiedAt: time.Now().UTC()}
	s.log("node_verified", map[string]any{"provider": provider, "group": groupName, "node": chosen})
	return chosen, true
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
