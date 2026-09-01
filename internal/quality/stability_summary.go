package quality

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/state"
)

var ErrStabilitySummary = errors.New("quality stability summary error")

// StabilitySummarizer reads mihomo's existing delay history and updates only
// identities that already have a validated IP report. It never calls delay,
// never creates public requests, and has no write/select method in its API.
type StabilitySummarizer struct {
	API     ReadOnlyMihomoAPI
	Reports *Store
	State   *state.Store
	Logger  Logger
	Now     func() time.Time
}

// Summarize heartbeats once, then processes targets in quality.order. A
// target/node failure is retained as an error while later targets/nodes are
// still attempted. A lost mihomo link is returned immediately before any
// source/provider read.
func (s *StabilitySummarizer) Summarize(ctx context.Context, cfg config.Config) error {
	if err := s.heartbeat(ctx); err != nil {
		return err
	}
	if !cfg.Quality.Enabled {
		return nil
	}
	if s.Reports == nil {
		return fmt.Errorf("%w: report store is missing", ErrStabilitySummary)
	}

	byID := make(map[string]config.QualityTarget, len(cfg.Quality.Targets))
	for _, target := range cfg.Quality.Targets {
		byID[target.ID] = target
	}
	records, err := s.Reports.ListNodeRecords()
	if err != nil {
		return fmt.Errorf("%w: list existing node identities: %v", ErrStabilitySummary, err)
	}

	var firstErr error
	for _, targetID := range cfg.Quality.Order {
		target, ok := byID[targetID]
		if !ok {
			err := fmt.Errorf("%w: target %q is not configured", ErrStabilitySummary, targetID)
			s.log("quality_stability_target_failed", map[string]any{"target": targetID, "error": err.Error()})
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.summarizeTarget(ctx, cfg, target, records); err != nil {
			s.log("quality_stability_target_failed", map[string]any{"target": target.ID, "error": err.Error()})
			if errors.Is(err, ErrQualityLink) {
				return err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SummarizeTarget is the one-target entry point used by operators and tests.
// It repeats the heartbeat guard so a direct call cannot read mihomo after a
// control-plane loss.
func (s *StabilitySummarizer) SummarizeTarget(ctx context.Context, cfg config.Config, target config.QualityTarget) error {
	if err := s.heartbeat(ctx); err != nil {
		return err
	}
	if !cfg.Quality.Enabled {
		return nil
	}
	if s.Reports == nil {
		return fmt.Errorf("%w: report store is missing", ErrStabilitySummary)
	}
	records, err := s.Reports.ListNodeRecords()
	if err != nil {
		return fmt.Errorf("%w: list existing node identities: %v", ErrStabilitySummary, err)
	}
	return s.summarizeTarget(ctx, cfg, target, records)
}

func (s *StabilitySummarizer) heartbeat(ctx context.Context) error {
	if s == nil || s.API == nil {
		return fmt.Errorf("%w: API dependency is missing", ErrQualityLink)
	}
	if err := s.API.Heartbeat(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrQualityLink, err)
	}
	return nil
}

func (s *StabilitySummarizer) summarizeTarget(ctx context.Context, cfg config.Config, target config.QualityTarget, records []NodeRecord) error {
	candidates, err := resolveQualityCandidates(ctx, s.API, s.State, target)
	if err != nil {
		return fmt.Errorf("%w: target %q: %v", ErrStabilitySummary, target.ID, err)
	}
	var firstErr error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		proxy := candidate.providerNode
		if !candidate.hasProvider {
			proxy, err = s.API.GetProxy(ctx, candidate.name)
			if err != nil {
				nodeErr := fmt.Errorf("%w: target %q node %q read proxy: %v", ErrStabilitySummary, target.ID, candidate.name, err)
				s.log("quality_stability_node_failed", map[string]any{"target": target.ID, "node": candidate.name, "error": nodeErr.Error()})
				if firstErr == nil {
					firstErr = nodeErr
				}
				continue
			}
		}

		key, ok := existingIdentity(records, target, candidate.name, proxy)
		if !ok {
			// A history sample without a prior two-source IP identity cannot
			// establish a safe baseline. Leave it out until the next full scan.
			s.log("quality_stability_identity_missing", map[string]any{
				"target": target.ID, "provider": firstNonEmpty(firstNonEmpty(target.Provider, proxy.ProviderName), target.SourceGroup), "node": candidate.name,
			})
			continue
		}

		now := s.now()
		snapshot := AggregateStability([]mihomo.Proxy{proxy}, candidate.name, now, cfg.Quality.Stability)
		snapshot.Identity = key
		if _, err := s.Reports.ApplyStability(key, snapshot, proxy.Alive, minimumConfidence(cfg.Quality)); err != nil {
			nodeErr := fmt.Errorf("%w: target %q node %q apply snapshot: %v", ErrStabilitySummary, target.ID, candidate.name, err)
			s.log("quality_stability_node_failed", map[string]any{"target": target.ID, "node": candidate.name, "error": nodeErr.Error()})
			if firstErr == nil {
				firstErr = nodeErr
			}
			continue
		}
		s.log("quality_stability_node_complete", map[string]any{
			"target": target.ID, "provider": key.Provider, "node": candidate.name,
			"samples": snapshot.Samples, "known": snapshot.Known, "fresh": snapshot.Fresh,
			"stability_score": snapshot.StabilityScore,
		})
	}
	s.log("quality_stability_target_complete", map[string]any{"target": target.ID, "nodes": len(candidates)})
	return firstErr
}

func existingIdentity(records []NodeRecord, target config.QualityTarget, node string, proxy mihomo.Proxy) (NodeKey, bool) {
	provider := firstNonEmpty(firstNonEmpty(target.Provider, proxy.ProviderName), target.SourceGroup)
	var selected NodeRecord
	selectedAt := time.Time{}
	for _, record := range records {
		key := record.Identity.Canonical()
		if key.Target != target.ID || key.Provider != provider || key.Node != node {
			continue
		}
		when := record.UpdatedAt
		if record.Latest != nil {
			if record.Latest.StabilityObservedAt.After(when) {
				when = record.Latest.StabilityObservedAt
			}
			if record.Latest.ObservedAt.After(when) {
				when = record.Latest.ObservedAt
			}
		}
		if !selectedAt.IsZero() && !when.After(selectedAt) {
			continue
		}
		selected = record
		selectedAt = when
	}
	if selected.Identity.IP == "" || strings.TrimSpace(selected.Identity.IPFamily) == "" {
		return NodeKey{}, false
	}
	return selected.Identity.Canonical(), true
}

func (s *StabilitySummarizer) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *StabilitySummarizer) log(event string, fields map[string]any) {
	if s == nil || s.Logger == nil {
		return
	}
	_ = s.Logger.Event(event, fields)
}
