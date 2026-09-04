package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mihomo    MihomoConfig    `yaml:"mihomo"`
	Groups    GroupsConfig    `yaml:"groups"`
	Providers ProvidersConfig `yaml:"providers"`
	Decision  DecisionConfig  `yaml:"decision"`
	Probes    []ProbeSpec     `yaml:"probes"`
	Purity    PurityConfig    `yaml:"purity"`
	Quality   QualityConfig   `yaml:"quality"`
	Logging   LoggingConfig   `yaml:"logging"`
	Reload    ReloadConfig    `yaml:"reload"`
}

type MihomoConfig struct {
	API        string `yaml:"api"`
	Proxy      string `yaml:"proxy"`
	SecretFile string `yaml:"secret_file"`
}

type GroupsConfig struct {
	Channel string `yaml:"channel"`
	Main    string `yaml:"main"`
	Backup  string `yaml:"backup"`
}

type ProvidersConfig struct {
	Main   string `yaml:"main"`
	Backup string `yaml:"backup"`
}

type DecisionConfig struct {
	Mode                        string        `yaml:"mode"`
	Interval                    time.Duration `yaml:"-"`
	ProbeInterval               time.Duration `yaml:"-"`
	FailureRecheckInterval      time.Duration `yaml:"-"`
	RecoveryHealthcheckInterval time.Duration `yaml:"-"`
	FailuresBeforeSwitch        int           `yaml:"failures_before_switch"`
	RecoveriesBeforeSwitch      int           `yaml:"recoveries_before_switch"`
	MinHold                     time.Duration `yaml:"-"`
	LinkLossGrace               time.Duration `yaml:"-"`
	StartupAPITimeout           time.Duration `yaml:"-"`
	CriticalQuorum              int           `yaml:"critical_quorum"`
}

type ProbeSpec struct {
	ID           string        `yaml:"id"`
	URL          string        `yaml:"url"`
	Method       string        `yaml:"method"`
	Critical     bool          `yaml:"critical"`
	Enabled      bool          `yaml:"enabled"`
	Timeout      time.Duration `yaml:"-"`
	ExpectedMin  int           `yaml:"expected_min"`
	ExpectedMax  int           `yaml:"expected_max"`
	DelayTimeout time.Duration `yaml:"-"`
}

type PurityConfig struct {
	Enabled         bool           `yaml:"enabled"`
	AutomaticSwitch bool           `yaml:"automatic_switch"`
	URLs            []string       `yaml:"urls"`
	Sources         []PuritySource `yaml:"sources"`
	Timeout         time.Duration  `yaml:"-"`
}

// PuritySource is an explicitly configured public evidence endpoint. Keeping
// kind and format in the behavior file avoids guessing whether an endpoint is
// an identity or reputation source from its URL spelling.
type PuritySource struct {
	ID       string `yaml:"id"`
	URL      string `yaml:"url"`
	Kind     string `yaml:"kind"`
	Format   string `yaml:"format"`
	Critical bool   `yaml:"critical"`
}

type QualityConfig struct {
	Enabled          bool                   `yaml:"enabled"`
	FullScanInterval time.Duration          `yaml:"-"`
	RetryInterval    time.Duration          `yaml:"-"`
	Order            []string               `yaml:"order"`
	Targets          []QualityTarget        `yaml:"targets"`
	PerNodeTimeout   time.Duration          `yaml:"-"`
	Thresholds       QualityThresholds      `yaml:"thresholds"`
	Stability        QualityStabilityConfig `yaml:"stability"`
	Retention        QualityRetentionConfig `yaml:"retention"`
}

type QualityTarget struct {
	ID          string `yaml:"id"`
	SourceGroup string `yaml:"source_group"`
	Provider    string `yaml:"provider"`
	Scope       string `yaml:"scope"`
	LockKey     string `yaml:"lock_key"`
	NodeFilter  string `yaml:"node_filter"`
	Listener    string `yaml:"listener"`
}

type QualityThresholds struct {
	BaselineDropPoints    int `yaml:"baseline_drop_points"`
	MinimumConfidence     int `yaml:"minimum_confidence"`
	CandidateMinimumScore int `yaml:"candidate_minimum_score"`
	RecoveryMarginPoints  int `yaml:"recovery_margin_points"`
	RecoveryConfirmations int `yaml:"recovery_confirmations"`
}

type QualityStabilityConfig struct {
	SummaryInterval time.Duration `yaml:"-"`
	HistoryWindow   time.Duration `yaml:"-"`
	MinimumSamples  int           `yaml:"minimum_samples"`
	MinimumCoverage int           `yaml:"minimum_coverage_percent"`
	StaleAfter      time.Duration `yaml:"-"`
	GoodLatencyMS   int           `yaml:"good_latency_ms"`
	BadLatencyMS    int           `yaml:"bad_latency_ms"`
}

type QualityRetentionConfig struct {
	Reports     int `yaml:"reports"`
	HistoryDays int `yaml:"history_days"`
}

type LoggingConfig struct {
	MaxBytes int `yaml:"max_bytes"`
	Retain   int `yaml:"retain"`
}

type ReloadConfig struct {
	CheckInterval time.Duration `yaml:"-"`
}

type rawConfig struct {
	Mihomo    MihomoConfig    `yaml:"mihomo"`
	Groups    GroupsConfig    `yaml:"groups"`
	Providers ProvidersConfig `yaml:"providers"`
	Decision  rawDecision     `yaml:"decision"`
	Probes    []rawProbe      `yaml:"probes"`
	Purity    rawPurity       `yaml:"purity"`
	Quality   *rawQuality     `yaml:"quality"`
	Logging   LoggingConfig   `yaml:"logging"`
	Reload    rawReload       `yaml:"reload"`
}

type rawDecision struct {
	Mode                        string `yaml:"mode"`
	Interval                    string `yaml:"interval"`
	ProbeInterval               string `yaml:"probe_interval"`
	FailureRecheckInterval      string `yaml:"failure_recheck_interval"`
	RecoveryHealthcheckInterval string `yaml:"recovery_healthcheck_interval"`
	FailuresBeforeSwitch        int    `yaml:"failures_before_switch"`
	RecoveriesBeforeSwitch      int    `yaml:"recoveries_before_switch"`
	MinHold                     string `yaml:"min_hold"`
	LinkLossGrace               string `yaml:"link_loss_grace"`
	StartupAPITimeout           string `yaml:"startup_api_timeout"`
	CriticalQuorum              int    `yaml:"critical_quorum"`
}

type rawProbe struct {
	ID           string `yaml:"id"`
	URL          string `yaml:"url"`
	Method       string `yaml:"method"`
	Critical     bool   `yaml:"critical"`
	Enabled      *bool  `yaml:"enabled"`
	Timeout      string `yaml:"timeout"`
	ExpectedMin  int    `yaml:"expected_min"`
	ExpectedMax  int    `yaml:"expected_max"`
	DelayTimeout string `yaml:"delay_timeout"`
}

type rawPurity struct {
	Enabled         bool              `yaml:"enabled"`
	AutomaticSwitch bool              `yaml:"automatic_switch"`
	URLs            []string          `yaml:"urls"`
	Sources         []rawPuritySource `yaml:"sources"`
	Timeout         string            `yaml:"timeout"`
}

type rawPuritySource struct {
	ID       string `yaml:"id"`
	URL      string `yaml:"url"`
	Kind     string `yaml:"kind"`
	Format   string `yaml:"format"`
	Critical bool   `yaml:"critical"`
}

type rawQuality struct {
	Enabled          *bool                `yaml:"enabled"`
	FullScanInterval string               `yaml:"full_scan_interval"`
	RetryInterval    string               `yaml:"retry_interval"`
	Order            []string             `yaml:"order"`
	Targets          []rawQualityTarget   `yaml:"targets"`
	PerNodeTimeout   string               `yaml:"per_node_timeout"`
	Thresholds       rawQualityThresholds `yaml:"thresholds"`
	Stability        rawQualityStability  `yaml:"stability"`
	Retention        rawQualityRetention  `yaml:"retention"`
}

type rawQualityTarget struct {
	ID          string `yaml:"id"`
	SourceGroup string `yaml:"source_group"`
	Provider    string `yaml:"provider"`
	Scope       string `yaml:"scope"`
	LockKey     string `yaml:"lock_key"`
	NodeFilter  string `yaml:"node_filter"`
	Listener    string `yaml:"listener"`
}

type rawQualityThresholds struct {
	BaselineDropPoints    *int `yaml:"baseline_drop_points"`
	MinimumConfidence     *int `yaml:"minimum_confidence"`
	CandidateMinimumScore *int `yaml:"candidate_minimum_score"`
	RecoveryMarginPoints  *int `yaml:"recovery_margin_points"`
	RecoveryConfirmations *int `yaml:"recovery_confirmations"`
}

type rawQualityStability struct {
	SummaryInterval string `yaml:"summary_interval"`
	HistoryWindow   string `yaml:"history_window"`
	MinimumSamples  *int   `yaml:"minimum_samples"`
	MinimumCoverage *int   `yaml:"minimum_coverage_percent"`
	StaleAfter      string `yaml:"stale_after"`
	GoodLatencyMS   *int   `yaml:"good_latency_ms"`
	BadLatencyMS    *int   `yaml:"bad_latency_ms"`
}

type rawQualityRetention struct {
	Reports     *int `yaml:"reports"`
	HistoryDays *int `yaml:"history_days"`
}

type rawReload struct {
	CheckInterval string `yaml:"check_interval"`
}

func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return LoadBytes(data)
}

func LoadBytes(data []byte) (Config, error) {
	var raw rawConfig
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return normalize(raw)
}

func normalize(raw rawConfig) (Config, error) {
	cfg := Config{
		Mihomo:    raw.Mihomo,
		Groups:    raw.Groups,
		Providers: raw.Providers,
		Decision: DecisionConfig{
			Mode:                   raw.Decision.Mode,
			FailuresBeforeSwitch:   raw.Decision.FailuresBeforeSwitch,
			RecoveriesBeforeSwitch: raw.Decision.RecoveriesBeforeSwitch,
			CriticalQuorum:         raw.Decision.CriticalQuorum,
		},
		Logging: raw.Logging,
	}
	var err error
	cfg.Decision.Interval, err = durationOrDefault(raw.Decision.Interval, 15*time.Second)
	if err != nil {
		return Config{}, fieldError("decision.interval", err)
	}
	cfg.Decision.ProbeInterval, err = durationOrDefault(raw.Decision.ProbeInterval, 5*time.Minute)
	if err != nil {
		return Config{}, fieldError("decision.probe_interval", err)
	}
	cfg.Decision.FailureRecheckInterval, err = durationOrDefault(raw.Decision.FailureRecheckInterval, 30*time.Second)
	if err != nil {
		return Config{}, fieldError("decision.failure_recheck_interval", err)
	}
	cfg.Decision.RecoveryHealthcheckInterval, err = durationOrDefault(raw.Decision.RecoveryHealthcheckInterval, 2*time.Minute)
	if err != nil {
		return Config{}, fieldError("decision.recovery_healthcheck_interval", err)
	}
	cfg.Decision.MinHold, err = durationOrDefault(raw.Decision.MinHold, 120*time.Second)
	if err != nil {
		return Config{}, fieldError("decision.min_hold", err)
	}
	cfg.Decision.LinkLossGrace, err = durationOrDefault(raw.Decision.LinkLossGrace, 15*time.Second)
	if err != nil {
		return Config{}, fieldError("decision.link_loss_grace", err)
	}
	cfg.Decision.StartupAPITimeout, err = durationOrDefault(raw.Decision.StartupAPITimeout, 60*time.Second)
	if err != nil {
		return Config{}, fieldError("decision.startup_api_timeout", err)
	}
	if cfg.Decision.Mode == "" {
		cfg.Decision.Mode = "auto"
	}
	if cfg.Decision.FailuresBeforeSwitch == 0 {
		cfg.Decision.FailuresBeforeSwitch = 3
	}
	if cfg.Decision.RecoveriesBeforeSwitch == 0 {
		cfg.Decision.RecoveriesBeforeSwitch = 2
	}
	if cfg.Decision.CriticalQuorum == 0 {
		cfg.Decision.CriticalQuorum = 2
	}

	for _, item := range raw.Probes {
		probe := ProbeSpec{ID: item.ID, URL: item.URL, Method: item.Method, Critical: item.Critical, Enabled: true, ExpectedMin: item.ExpectedMin, ExpectedMax: item.ExpectedMax}
		if item.Enabled != nil {
			probe.Enabled = *item.Enabled
		}
		probe.Timeout, err = durationOrDefault(item.Timeout, 5*time.Second)
		if err != nil {
			return Config{}, fieldError("probes."+item.ID+".timeout", err)
		}
		probe.DelayTimeout, err = durationOrDefault(item.DelayTimeout, 5*time.Second)
		if err != nil {
			return Config{}, fieldError("probes."+item.ID+".delay_timeout", err)
		}
		if probe.Method == "" {
			probe.Method = "GET"
		}
		if probe.ExpectedMin == 0 {
			probe.ExpectedMin = 200
		}
		if probe.ExpectedMax == 0 {
			probe.ExpectedMax = 499
		}
		cfg.Probes = append(cfg.Probes, probe)
	}

	cfg.Purity.Enabled = raw.Purity.Enabled
	cfg.Purity.AutomaticSwitch = raw.Purity.AutomaticSwitch
	cfg.Purity.URLs = append([]string(nil), raw.Purity.URLs...)
	for _, source := range raw.Purity.Sources {
		cfg.Purity.Sources = append(cfg.Purity.Sources, PuritySource{
			ID: source.ID, URL: source.URL, Kind: source.Kind, Format: source.Format, Critical: source.Critical,
		})
	}
	cfg.Purity.Timeout, err = durationOrDefault(raw.Purity.Timeout, 5*time.Second)
	if err != nil {
		return Config{}, fieldError("purity.timeout", err)
	}
	cfg.Quality, err = normalizeQuality(raw.Quality)
	if err != nil {
		return Config{}, err
	}
	cfg.Reload.CheckInterval, err = durationOrDefault(raw.Reload.CheckInterval, 2*time.Second)
	if err != nil {
		return Config{}, fieldError("reload.check_interval", err)
	}
	if cfg.Logging.MaxBytes == 0 {
		cfg.Logging.MaxBytes = 10 * 1024 * 1024
	}
	if cfg.Logging.Retain == 0 {
		cfg.Logging.Retain = 7
	}

	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeQuality(raw *rawQuality) (QualityConfig, error) {
	if raw == nil {
		raw = &rawQuality{}
	}

	quality := QualityConfig{}
	if raw.Enabled != nil {
		quality.Enabled = *raw.Enabled
	}
	var err error
	quality.FullScanInterval, err = durationOrDefault(raw.FullScanInterval, 720*time.Hour)
	if err != nil {
		return QualityConfig{}, fieldError("quality.full_scan_interval", err)
	}
	quality.RetryInterval, err = durationOrDefault(raw.RetryInterval, 24*time.Hour)
	if err != nil {
		return QualityConfig{}, fieldError("quality.retry_interval", err)
	}
	quality.PerNodeTimeout, err = durationOrDefault(raw.PerNodeTimeout, 180*time.Second)
	if err != nil {
		return QualityConfig{}, fieldError("quality.per_node_timeout", err)
	}
	quality.Order = append([]string(nil), raw.Order...)
	for _, target := range raw.Targets {
		quality.Targets = append(quality.Targets, QualityTarget{
			ID:          target.ID,
			SourceGroup: target.SourceGroup,
			Provider:    target.Provider,
			Scope:       target.Scope,
			LockKey:     target.LockKey,
			NodeFilter:  target.NodeFilter,
			Listener:    target.Listener,
		})
	}

	quality.Thresholds = QualityThresholds{
		BaselineDropPoints:    intOrDefault(raw.Thresholds.BaselineDropPoints, 20),
		MinimumConfidence:     intOrDefault(raw.Thresholds.MinimumConfidence, 70),
		CandidateMinimumScore: intOrDefault(raw.Thresholds.CandidateMinimumScore, 60),
		RecoveryMarginPoints:  intOrDefault(raw.Thresholds.RecoveryMarginPoints, 10),
		RecoveryConfirmations: intOrDefault(raw.Thresholds.RecoveryConfirmations, 2),
	}
	quality.Stability.SummaryInterval, err = durationOrDefault(raw.Stability.SummaryInterval, time.Hour)
	if err != nil {
		return QualityConfig{}, fieldError("quality.stability.summary_interval", err)
	}
	quality.Stability.HistoryWindow, err = durationOrDefault(raw.Stability.HistoryWindow, 24*time.Hour)
	if err != nil {
		return QualityConfig{}, fieldError("quality.stability.history_window", err)
	}
	quality.Stability.StaleAfter, err = durationOrDefault(raw.Stability.StaleAfter, 26*time.Hour)
	if err != nil {
		return QualityConfig{}, fieldError("quality.stability.stale_after", err)
	}
	quality.Stability.MinimumSamples = intOrDefault(raw.Stability.MinimumSamples, 3)
	quality.Stability.MinimumCoverage = intOrDefault(raw.Stability.MinimumCoverage, 10)
	quality.Stability.GoodLatencyMS = intOrDefault(raw.Stability.GoodLatencyMS, 500)
	quality.Stability.BadLatencyMS = intOrDefault(raw.Stability.BadLatencyMS, 3000)
	quality.Retention.Reports = intOrDefault(raw.Retention.Reports, 90)
	quality.Retention.HistoryDays = intOrDefault(raw.Retention.HistoryDays, 180)

	return quality, nil
}

func intOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func durationOrDefault(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, errors.New("must be a positive duration")
	}
	return d, nil
}

func fieldError(field string, err error) error { return fmt.Errorf("%s: %w", field, err) }

func validate(cfg Config) error {
	if err := validateLoopbackURL(cfg.Mihomo.API, "mihomo api", false); err != nil {
		return err
	}
	if err := validateProxyURL(cfg.Mihomo.Proxy); err != nil {
		return err
	}
	// The command entrypoints provide /guardian/controller_secret by default.
	// Keep this field optional so the installer can use a separate persistent
	// secret mount without duplicating its path into generated configuration.
	if cfg.Groups.Channel == "" || cfg.Groups.Main == "" || cfg.Groups.Backup == "" {
		return errors.New("mihomo groups channel, main, and backup are required")
	}
	if (cfg.Providers.Main == "") != (cfg.Providers.Backup == "") {
		return errors.New("providers main and backup must be configured together")
	}
	if cfg.Decision.Mode != "auto" && cfg.Decision.Mode != "observe" && cfg.Decision.Mode != "force" {
		return errors.New("decision.mode must be auto, observe, or force")
	}
	if cfg.Decision.FailuresBeforeSwitch < 1 || cfg.Decision.RecoveriesBeforeSwitch < 1 || cfg.Decision.CriticalQuorum < 1 {
		return errors.New("decision thresholds must be positive")
	}
	if cfg.Decision.LinkLossGrace < time.Second {
		return errors.New("decision.link_loss_grace must be at least 1s")
	}
	if cfg.Logging.MaxBytes < 1024 || cfg.Logging.Retain < 1 {
		return errors.New("logging limits are too small")
	}
	if err := validateQuality(cfg.Quality, cfg.Mihomo); err != nil {
		return err
	}

	ids := make(map[string]struct{})
	critical := 0
	for _, probe := range cfg.Probes {
		if probe.ID == "" || probe.URL == "" {
			return errors.New("each probe requires id and url")
		}
		if _, exists := ids[probe.ID]; exists {
			return fmt.Errorf("duplicate probe id %q", probe.ID)
		}
		ids[probe.ID] = struct{}{}
		parsed, err := url.Parse(probe.URL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Hostname() == "" {
			return fmt.Errorf("probe %q has invalid url", probe.ID)
		}
		if probe.ExpectedMin < 100 || probe.ExpectedMax > 599 || probe.ExpectedMin > probe.ExpectedMax {
			return fmt.Errorf("probe %q has invalid expected status range", probe.ID)
		}
		if probe.Enabled && probe.Critical {
			critical++
		}
	}
	if critical == 0 {
		return errors.New("at least one enabled critical probe is required")
	}
	if cfg.Decision.CriticalQuorum > critical {
		return fmt.Errorf("critical_quorum %d exceeds enabled critical probes %d", cfg.Decision.CriticalQuorum, critical)
	}
	for _, rawURL := range cfg.Purity.URLs {
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
			return fmt.Errorf("purity url %q must be an https url", rawURL)
		}
	}
	sourceIDs := make(map[string]struct{}, len(cfg.Purity.Sources))
	for _, source := range cfg.Purity.Sources {
		if strings.TrimSpace(source.ID) == "" || strings.TrimSpace(source.URL) == "" {
			return errors.New("each purity source requires id and url")
		}
		sourceID := strings.ToLower(strings.TrimSpace(source.ID))
		if _, exists := sourceIDs[sourceID]; exists {
			return fmt.Errorf("duplicate purity source id %q", source.ID)
		}
		sourceIDs[sourceID] = struct{}{}
		parsed, err := url.Parse(source.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return fmt.Errorf("purity source %q must be an https URL without credentials", source.ID)
		}
		switch source.Kind {
		case "ip", "identity", "risk":
		default:
			return fmt.Errorf("purity source %q kind must be ip, identity, or risk", source.ID)
		}
		switch source.Format {
		case "text", "json":
		default:
			return fmt.Errorf("purity source %q format must be text or json", source.ID)
		}
	}
	return nil
}

var qualityTargetIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func validateQuality(quality QualityConfig, mihomo MihomoConfig) error {
	if quality.Enabled && len(quality.Targets) == 0 {
		return errors.New("quality.enabled requires at least one target")
	}

	targets := make(map[string]QualityTarget, len(quality.Targets))
	listeners := make(map[int]string, len(quality.Targets))
	mihomoPorts := map[int]string{}
	if port, ok := configuredPort(mihomo.API); ok {
		mihomoPorts[port] = "mihomo api"
	}
	if port, ok := configuredPort(mihomo.Proxy); ok {
		if existing, exists := mihomoPorts[port]; exists {
			mihomoPorts[port] = existing + " and mihomo proxy"
		} else {
			mihomoPorts[port] = "mihomo proxy"
		}
	}
	for _, target := range quality.Targets {
		if !qualityTargetIDPattern.MatchString(target.ID) {
			return fmt.Errorf("invalid quality target id %q", target.ID)
		}
		if _, exists := targets[target.ID]; exists {
			return fmt.Errorf("duplicate quality target id %q", target.ID)
		}
		if strings.TrimSpace(target.SourceGroup) == "" {
			return fmt.Errorf("quality target %q requires source_group", target.ID)
		}
		if target.Scope != "locked" && target.Scope != "all" {
			return fmt.Errorf("quality target %q scope must be locked or all", target.ID)
		}
		if target.Scope == "locked" && strings.TrimSpace(target.LockKey) == "" {
			return fmt.Errorf("quality target %q requires lock_key for locked scope", target.ID)
		}
		port, err := validateQualityListener(target.Listener)
		if err != nil {
			return fmt.Errorf("quality target %q listener: %w", target.ID, err)
		}
		if endpoint, exists := mihomoPorts[port]; exists {
			return fmt.Errorf("quality target %q listener port %d conflicts with %s port %d", target.ID, port, endpoint, port)
		}
		if previous, exists := listeners[port]; exists {
			return fmt.Errorf("duplicate quality listener port %d for targets %q and %q", port, previous, target.ID)
		}
		listeners[port] = target.ID
		if target.NodeFilter != "" {
			if _, err := regexp.Compile(target.NodeFilter); err != nil {
				return fmt.Errorf("quality target %q node_filter: %w", target.ID, err)
			}
		}
		targets[target.ID] = target
	}

	seen := make(map[string]struct{}, len(quality.Order))
	for _, id := range quality.Order {
		if _, exists := targets[id]; !exists {
			return fmt.Errorf("quality.order references unknown target %q", id)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("quality.order contains duplicate target %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(quality.Order) != len(quality.Targets) {
		return errors.New("each quality target must appear exactly once in quality.order")
	}
	for id := range targets {
		if _, exists := seen[id]; !exists {
			return fmt.Errorf("quality target %q must appear exactly once in quality.order", id)
		}
	}

	thresholds := quality.Thresholds
	if thresholds.BaselineDropPoints < 1 || thresholds.BaselineDropPoints > 100 {
		return errors.New("quality.thresholds.baseline_drop_points must be between 1 and 100")
	}
	if thresholds.MinimumConfidence < 0 || thresholds.MinimumConfidence > 100 {
		return errors.New("quality.thresholds.minimum_confidence must be between 0 and 100")
	}
	if thresholds.CandidateMinimumScore < 0 || thresholds.CandidateMinimumScore > 100 {
		return errors.New("quality.thresholds.candidate_minimum_score must be between 0 and 100")
	}
	if thresholds.RecoveryMarginPoints < 0 || thresholds.RecoveryMarginPoints > 100 {
		return errors.New("quality.thresholds.recovery_margin_points must be between 0 and 100")
	}
	if thresholds.RecoveryConfirmations < 1 {
		return errors.New("quality.thresholds.recovery_confirmations must be positive")
	}

	stability := quality.Stability
	if stability.MinimumSamples < 1 {
		return errors.New("quality.stability.minimum_samples must be positive")
	}
	if stability.MinimumCoverage < 1 || stability.MinimumCoverage > 100 {
		return errors.New("quality.stability.minimum_coverage_percent must be between 1 and 100")
	}
	if stability.GoodLatencyMS < 1 || stability.BadLatencyMS < 1 || stability.BadLatencyMS <= stability.GoodLatencyMS {
		return errors.New("quality.stability latency thresholds must be positive and bad_latency_ms greater than good_latency_ms")
	}
	if quality.Retention.Reports < 1 || quality.Retention.HistoryDays < 1 {
		return errors.New("quality retention reports and history_days must be positive")
	}
	return nil
}

func configuredPort(raw string) (int, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Port() == "" {
		return 0, false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func validateQualityListener(raw string) (int, error) {
	listener, err := url.Parse(raw)
	if err != nil || listener.Scheme != "http" || listener.Hostname() == "" || listener.User != nil || (listener.Path != "" && listener.Path != "/") || listener.RawQuery != "" || listener.Fragment != "" {
		return 0, errors.New("must be an http loopback URL")
	}
	if !isLoopbackHost(listener.Hostname()) {
		return 0, errors.New("must point to loopback")
	}
	portText := listener.Port()
	port, err := strconv.Atoi(portText)
	if portText == "" || err != nil || port < 1 || port > 65535 {
		return 0, errors.New("must include a valid port")
	}
	return port, nil
}

func validateProxyURL(raw string) error {
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Scheme == "" || proxy.Hostname() == "" {
		return errors.New("mihomo proxy must be a loopback http(s) or socks5 URL")
	}
	if proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h" {
		return errors.New("mihomo proxy must be a loopback http(s) or socks5 URL")
	}
	if !isLoopbackHost(proxy.Hostname()) {
		return errors.New("mihomo proxy must point to loopback")
	}
	port, err := strconv.Atoi(proxy.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("mihomo proxy must include a valid port")
	}
	return nil
}

func validateLoopbackURL(raw, name string, requirePort bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" {
		return fmt.Errorf("%s must be an http loopback URL", name)
	}
	if !isLoopbackHost(parsed.Hostname()) {
		return fmt.Errorf("%s must point to loopback", name)
	}
	if requirePort && parsed.Port() == "" {
		return fmt.Errorf("%s must include a port", name)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type Loader struct {
	path    string
	mu      sync.Mutex
	modTime time.Time
	size    int64
}

func NewLoader(path string) *Loader { return &Loader{path: path} }

func (l *Loader) Load() (Config, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cfg, err := LoadFile(l.path)
	if err != nil {
		return Config{}, err
	}
	if err := l.rememberStat(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (l *Loader) ReloadIfChanged(previous Config) (Config, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.path)
	if err != nil {
		return previous, false, err
	}
	if info.ModTime().Equal(l.modTime) && info.Size() == l.size {
		return previous, false, nil
	}
	cfg, err := LoadFile(l.path)
	if err != nil {
		return previous, false, err
	}
	l.modTime, l.size = info.ModTime(), info.Size()
	return cfg, true, nil
}

func (l *Loader) rememberStat() error {
	info, err := os.Stat(l.path)
	if err != nil {
		return err
	}
	l.modTime, l.size = info.ModTime(), info.Size()
	return nil
}
