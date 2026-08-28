package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
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
	Mode                   string        `yaml:"mode"`
	Interval               time.Duration `yaml:"-"`
	FailuresBeforeSwitch   int           `yaml:"failures_before_switch"`
	RecoveriesBeforeSwitch int           `yaml:"recoveries_before_switch"`
	MinHold                time.Duration `yaml:"-"`
	LinkLossGrace          time.Duration `yaml:"-"`
	StartupAPITimeout      time.Duration `yaml:"-"`
	CriticalQuorum         int           `yaml:"critical_quorum"`
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
	Enabled         bool          `yaml:"enabled"`
	AutomaticSwitch bool          `yaml:"automatic_switch"`
	URLs            []string      `yaml:"urls"`
	Timeout         time.Duration `yaml:"-"`
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
	Logging   LoggingConfig   `yaml:"logging"`
	Reload    rawReload       `yaml:"reload"`
}

type rawDecision struct {
	Mode                   string `yaml:"mode"`
	Interval               string `yaml:"interval"`
	FailuresBeforeSwitch   int    `yaml:"failures_before_switch"`
	RecoveriesBeforeSwitch int    `yaml:"recoveries_before_switch"`
	MinHold                string `yaml:"min_hold"`
	LinkLossGrace          string `yaml:"link_loss_grace"`
	StartupAPITimeout      string `yaml:"startup_api_timeout"`
	CriticalQuorum         int    `yaml:"critical_quorum"`
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
	Enabled         bool     `yaml:"enabled"`
	AutomaticSwitch bool     `yaml:"automatic_switch"`
	URLs            []string `yaml:"urls"`
	Timeout         string   `yaml:"timeout"`
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
	cfg.Purity.Timeout, err = durationOrDefault(raw.Purity.Timeout, 5*time.Second)
	if err != nil {
		return Config{}, fieldError("purity.timeout", err)
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
	return nil
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
