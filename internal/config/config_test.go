package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validMinimalConfig(t *testing.T) []byte {
	t.Helper()
	return []byte(`
mihomo:
  api: http://127.0.0.1:9090
  proxy: http://127.0.0.1:7890
  secret_file: /tmp/secret
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
decision:
  interval: 15s
  failures_before_switch: 3
  recoveries_before_switch: 2
  min_hold: 120s
  link_loss_grace: 15s
probes:
  - id: openai
    url: https://api.openai.com/v1/models
    critical: true
  - id: gemini
    url: https://generativelanguage.googleapis.com/v1beta/models
    critical: true
`)
}

func TestLoadRejectsExternalProbeWithoutMihomoProxy(t *testing.T) {
	_, err := LoadBytes([]byte(`
mihomo:
  api: http://127.0.0.1:9090
  proxy: ""
  secret_file: /tmp/secret
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
probes:
  - id: openai
    url: https://api.openai.com/v1/models
`))
	if err == nil || !strings.Contains(err.Error(), "mihomo proxy") {
		t.Fatalf("expected proxy validation error, got %v", err)
	}
}

func TestLoadAcceptsDiscoveredLoopbackProxyPort(t *testing.T) {
	data := []byte(`
mihomo:
  api: http://127.0.0.1:19090
  proxy: socks5://127.0.0.1:17892
  secret_file: /tmp/secret
groups:
  channel: CHANNEL
  main: MAIN
  backup: BACKUP-USA
decision:
  critical_quorum: 1
probes:
  - id: openai
    url: https://api.openai.com/v1/models
    critical: true
`)
	if _, err := LoadBytes(data); err != nil {
		t.Fatalf("discovered proxy port was rejected: %v", err)
	}
}

func TestLoadAllowsSecretToBeSuppliedByRunCommand(t *testing.T) {
	data := append(validMinimalConfig(t), []byte{}...)
	data = []byte(strings.Replace(string(data), "  secret_file: /tmp/secret\n", "", 1))
	if _, err := LoadBytes(data); err != nil {
		t.Fatalf("secret_file should be supplied by the command default: %v", err)
	}
}

func TestLoadAppliesConservativeDefaults(t *testing.T) {
	cfg, err := LoadBytes(validMinimalConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decision.Interval != 15*time.Second {
		t.Fatalf("interval=%s", cfg.Decision.Interval)
	}
	if cfg.Decision.FailuresBeforeSwitch != 3 {
		t.Fatalf("fail threshold=%d", cfg.Decision.FailuresBeforeSwitch)
	}
	if cfg.Decision.RecoveriesBeforeSwitch != 2 {
		t.Fatalf("recovery threshold=%d", cfg.Decision.RecoveriesBeforeSwitch)
	}
	if cfg.Decision.LinkLossGrace != 15*time.Second {
		t.Fatalf("link grace=%s", cfg.Decision.LinkLossGrace)
	}
	if cfg.Decision.StartupAPITimeout != 60*time.Second {
		t.Fatalf("startup timeout=%s", cfg.Decision.StartupAPITimeout)
	}
	if cfg.Decision.MinHold != 120*time.Second {
		t.Fatalf("min hold=%s", cfg.Decision.MinHold)
	}
}

func TestLoadDefaultsPublicProbeIntervalToFiveMinutes(t *testing.T) {
	data := []byte(strings.Replace(string(validMinimalConfig(t)), "  interval: 15s\n", "", 1))
	cfg, err := LoadBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decision.ProbeInterval != 5*time.Minute {
		t.Fatalf("public probe interval=%s, want 5m", cfg.Decision.ProbeInterval)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	data := append(validMinimalConfig(t), []byte("unknown: true\n")...)
	if _, err := LoadBytes(data); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func TestLoadPreservesDiscoveredProviderMapping(t *testing.T) {
	data := append(validMinimalConfig(t), []byte("providers:\n  main: main-channel\n  backup: backup-channel\n")...)
	cfg, err := LoadBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.Main != "main-channel" || cfg.Providers.Backup != "backup-channel" {
		t.Fatalf("providers=%+v", cfg.Providers)
	}
}

func TestLoadRejectsPartialProviderMapping(t *testing.T) {
	data := append(validMinimalConfig(t), []byte("providers:\n  main: main-channel\n")...)
	if _, err := LoadBytes(data); err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("expected partial provider mapping error, got %v", err)
	}
}

func TestReloadKeepsPreviousConfigWhenNewFileIsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guardian.yaml")
	if err := os.WriteFile(path, validMinimalConfig(t), 0600); err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(path)
	old, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("decision: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	current, changed, err := loader.ReloadIfChanged(old)
	if err == nil || changed || current.Decision.FailuresBeforeSwitch != old.Decision.FailuresBeforeSwitch {
		t.Fatalf("invalid reload was accepted: changed=%v err=%v", changed, err)
	}
}

func validQualityConfig(t *testing.T) []byte {
	t.Helper()
	return append(validMinimalConfig(t), []byte(`
quality:
  enabled: true
  order: [primary, reserve]
  targets:
    - id: primary
      source_group: MAIN
      provider: main-channel
      scope: locked
      lock_key: main
      listener: http://127.0.0.1:17990
    - id: reserve
      source_group: BACKUP-USA
      provider: backup-channel
      scope: all
      node_filter: "美国"
      listener: http://127.0.0.1:17991
`)...)
}

func TestQualityConfigUsesUserDefinedTargetOrderAndDefaults(t *testing.T) {
	cfg, err := LoadBytes(validQualityConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Quality.Order, []string{"primary", "reserve"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quality order=%v, want %v", got, want)
	}
	if len(cfg.Quality.Targets) != 2 || cfg.Quality.Targets[0].SourceGroup != "MAIN" || cfg.Quality.Targets[1].SourceGroup != "BACKUP-USA" {
		t.Fatalf("quality targets=%+v", cfg.Quality.Targets)
	}
	if cfg.Quality.Targets[0].Scope != "locked" || cfg.Quality.Targets[0].LockKey != "main" || cfg.Quality.Targets[1].Scope != "all" || cfg.Quality.Targets[1].NodeFilter != "美国" {
		t.Fatalf("quality target fields=%+v", cfg.Quality.Targets)
	}
	if cfg.Quality.FullScanInterval != 720*time.Hour || cfg.Quality.RetryInterval != 24*time.Hour || cfg.Quality.PerNodeTimeout != 180*time.Second {
		t.Fatalf("quality scan defaults=%s/%s/%s", cfg.Quality.FullScanInterval, cfg.Quality.RetryInterval, cfg.Quality.PerNodeTimeout)
	}
	if cfg.Quality.Stability.SummaryInterval != time.Hour || cfg.Quality.Stability.HistoryWindow != 24*time.Hour || cfg.Quality.Stability.StaleAfter != 26*time.Hour {
		t.Fatalf("quality stability defaults=%+v", cfg.Quality.Stability)
	}
	if cfg.Quality.Thresholds.BaselineDropPoints != 20 || cfg.Quality.Thresholds.MinimumConfidence != 70 || cfg.Quality.Thresholds.CandidateMinimumScore != 60 || cfg.Quality.Thresholds.RecoveryMarginPoints != 10 || cfg.Quality.Thresholds.RecoveryConfirmations != 2 {
		t.Fatalf("quality thresholds=%+v", cfg.Quality.Thresholds)
	}
	if cfg.Quality.Stability.MinimumSamples != 3 || cfg.Quality.Stability.MinimumCoverage != 10 || cfg.Quality.Stability.GoodLatencyMS != 500 || cfg.Quality.Stability.BadLatencyMS != 3000 {
		t.Fatalf("quality stability thresholds=%+v", cfg.Quality.Stability)
	}
	if cfg.Quality.Retention.Reports != 90 || cfg.Quality.Retention.HistoryDays != 180 {
		t.Fatalf("quality retention=%+v", cfg.Quality.Retention)
	}
}

func TestPuritySourcesParseExplicitRiskKindAndFormat(t *testing.T) {
	data := append(validMinimalConfig(t), []byte(`
purity:
  sources:
    - id: ip-consensus
      url: https://identity.example/ip
      kind: identity
      format: json
    - id: risk-a
      url: https://risk.example/check
      kind: risk
      format: json
`)...)
	cfg, err := LoadBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Purity.Sources) != 2 || cfg.Purity.Sources[1].Kind != "risk" || cfg.Purity.Sources[1].Format != "json" {
		t.Fatalf("purity sources=%+v", cfg.Purity.Sources)
	}
}

func TestPuritySourcesRejectAmbiguousOrUnsafeDefinitions(t *testing.T) {
	for _, edit := range []string{
		"kind: unknown",
		"format: yaml",
		"url: http://risk.example/check",
	} {
		t.Run(edit, func(t *testing.T) {
			data := append(validMinimalConfig(t), []byte("\npurity:\n  sources:\n    - id: risk-a\n      "+edit+"\n      kind: risk\n      format: json\n")...)
			if _, err := LoadBytes(data); err == nil {
				t.Fatalf("definition %q was accepted", edit)
			}
		})
	}
}

func TestQualityConfigParsesConfiguredDurations(t *testing.T) {
	data := string(validQualityConfig(t))
	data = strings.Replace(data, "  enabled: true\n", "  enabled: true\n  full_scan_interval: 48h\n  retry_interval: 90m\n  per_node_timeout: 45s\n", 1)
	data = strings.Replace(data, "  order: [primary, reserve]\n", "  order: [primary, reserve]\n  stability:\n    summary_interval: 30m\n    history_window: 12h\n    stale_after: 13h\n", 1)
	cfg, err := LoadBytes([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Quality.FullScanInterval != 48*time.Hour || cfg.Quality.RetryInterval != 90*time.Minute || cfg.Quality.PerNodeTimeout != 45*time.Second {
		t.Fatalf("quality scan durations=%s/%s/%s", cfg.Quality.FullScanInterval, cfg.Quality.RetryInterval, cfg.Quality.PerNodeTimeout)
	}
	if cfg.Quality.Stability.SummaryInterval != 30*time.Minute || cfg.Quality.Stability.HistoryWindow != 12*time.Hour || cfg.Quality.Stability.StaleAfter != 13*time.Hour {
		t.Fatalf("quality stability durations=%+v", cfg.Quality.Stability)
	}
}

func TestQualityConfigRejectsMihomoEndpointListenerPortConflicts(t *testing.T) {
	tests := []struct {
		name string
		port string
		want string
	}{
		{name: "api", port: "9090", want: "conflicts with mihomo api port 9090"},
		{name: "proxy", port: "7890", want: "conflicts with mihomo proxy port 7890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := strings.Replace(
				string(validQualityConfig(t)),
				"listener: http://127.0.0.1:17990",
				"listener: http://127.0.0.1:"+tt.port,
				1,
			)
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestQualityConfigRejectsNestedUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		edit string
	}{
		{
			name: "quality",
			edit: "quality:\n  unknown: true\n  enabled: true",
		},
		{
			name: "target",
			edit: "    - id: primary\n      unknown: true",
		},
		{
			name: "thresholds",
			edit: "quality:\n  thresholds:\n    unknown: true\n  enabled: true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := string(validQualityConfig(t))
			switch tt.name {
			case "quality", "thresholds":
				data = strings.Replace(data, "quality:\n  enabled: true", tt.edit, 1)
			case "target":
				data = strings.Replace(data, "    - id: primary", tt.edit, 1)
			}
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("error=%v, want nested unknown-field error", err)
			}
		})
	}
}

func TestQualityConfigRejectsInvalidAddedDurations(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "not-a-duration"} {
		t.Run(value, func(t *testing.T) {
			data := strings.Replace(
				string(validQualityConfig(t)),
				"quality:\n  enabled: true",
				"quality:\n  enabled: true\n  full_scan_interval: "+value,
				1,
			)
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), "quality.full_scan_interval") {
				t.Fatalf("error=%v, want invalid full_scan_interval error", err)
			}
		})
	}
}

func TestQualityConfigRejectsMissingSourceGroup(t *testing.T) {
	data := strings.Replace(string(validQualityConfig(t)), "      source_group: MAIN\n", "", 1)
	if _, err := LoadBytes([]byte(data)); err == nil || !strings.Contains(err.Error(), "requires source_group") {
		t.Fatalf("error=%v, want missing source_group error", err)
	}
}

func TestQualityConfigEnabledRequiresTargets(t *testing.T) {
	data := append(validMinimalConfig(t), []byte("quality:\n  enabled: true\n")...)
	if _, err := LoadBytes(data); err == nil || !strings.Contains(err.Error(), "quality.enabled requires at least one target") {
		t.Fatalf("expected enabled quality target error, got %v", err)
	}
}

func TestQualityConfigAllowsDisabledOrAbsentQuality(t *testing.T) {
	if _, err := LoadBytes(validMinimalConfig(t)); err != nil {
		t.Fatalf("quality absent should remain compatible: %v", err)
	}
	data := append(validMinimalConfig(t), []byte("quality:\n  enabled: false\n")...)
	if _, err := LoadBytes(data); err != nil {
		t.Fatalf("disabled quality should remain compatible: %v", err)
	}
}

func TestQualityConfigRejectsTargetOrderAndIdentityErrors(t *testing.T) {
	tests := []struct {
		name string
		edit string
		want string
	}{
		{name: "duplicate target id", edit: "      id: reserve\n      source_group: MAIN", want: "duplicate quality target id"},
		{name: "duplicate order id", edit: "order: [primary, primary]", want: "quality.order contains duplicate"},
		{name: "incomplete order", edit: "order: [primary]", want: "must appear exactly once"},
		{name: "missing locked key", edit: "      lock_key: main\n      listener: http://127.0.0.1:17990", want: "lock_key"},
		{name: "invalid id", edit: "      id: Primary", want: "invalid quality target id"},
		{name: "invalid scope", edit: "      scope: selected", want: "scope must be locked or all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := string(validQualityConfig(t))
			switch tt.name {
			case "duplicate target id":
				first := strings.Index(data, "    - id: reserve")
				data = data[:first] + strings.Replace(data[first:], "    - id: reserve", "    - id: primary", 1)
			case "missing locked key":
				data = strings.Replace(data, "      lock_key: main\n", "", 1)
			case "invalid id":
				data = strings.Replace(data, "    - id: primary", "    - id: Primary", 1)
			case "invalid scope":
				data = strings.Replace(data, "      scope: locked", "      scope: selected", 1)
			default:
				data = strings.Replace(data, "  order: [primary, reserve]", "  "+tt.edit, 1)
			}
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestQualityConfigRejectsInvalidListenersAndFilters(t *testing.T) {
	tests := []struct {
		name string
		edit string
		want string
	}{
		{name: "duplicate listener", edit: "      listener: http://127.0.0.1:17990", want: "duplicate quality listener"},
		{name: "non-loopback listener", edit: "      listener: http://192.0.2.1:17991", want: "must point to loopback"},
		{name: "missing listener port", edit: "      listener: http://127.0.0.1", want: "must include a valid port"},
		{name: "https listener", edit: "      listener: https://127.0.0.1:17991", want: "must be an http loopback URL"},
		{name: "invalid filter", edit: "      node_filter: \"[\"", want: "node_filter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := string(validQualityConfig(t))
			second := strings.Index(data, "    - id: reserve")
			if tt.name == "invalid filter" {
				data = strings.Replace(data, "      node_filter: \"美国\"", tt.edit, 1)
			} else {
				data = data[:second] + strings.Replace(data[second:], "      listener: http://127.0.0.1:17991", tt.edit, 1)
			}
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestQualityConfigRejectsInvalidThresholds(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{name: "baseline drop", field: "baseline_drop_points", value: "101", want: "baseline_drop_points"},
		{name: "confidence", field: "minimum_confidence", value: "-1", want: "minimum_confidence"},
		{name: "candidate score", field: "candidate_minimum_score", value: "101", want: "candidate_minimum_score"},
		{name: "confirmations", field: "recovery_confirmations", value: "0", want: "recovery_confirmations"},
		{name: "samples", field: "minimum_samples", value: "0", want: "minimum_samples"},
		{name: "coverage", field: "minimum_coverage_percent", value: "0", want: "minimum_coverage_percent"},
		{name: "reports retention", field: "reports", value: "0", want: "reports"},
		{name: "history retention", field: "history_days", value: "0", want: "history_days"},
		{name: "good latency", field: "good_latency_ms", value: "0", want: "good_latency_ms"},
		{name: "latency order", field: "bad_latency_ms", value: "400", want: "bad_latency_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := string(validQualityConfig(t))
			switch tt.field {
			case "reports", "history_days":
				data = strings.Replace(data, "      listener: http://127.0.0.1:17991\n", "      listener: http://127.0.0.1:17991\n  retention:\n    "+tt.field+": "+tt.value+"\n", 1)
			case "minimum_samples", "minimum_coverage_percent", "good_latency_ms", "bad_latency_ms":
				data = strings.Replace(data, "      listener: http://127.0.0.1:17991\n", "      listener: http://127.0.0.1:17991\n  stability:\n    "+tt.field+": "+tt.value+"\n", 1)
			default:
				data = strings.Replace(data, "      listener: http://127.0.0.1:17991\n", "      listener: http://127.0.0.1:17991\n  thresholds:\n    "+tt.field+": "+tt.value+"\n", 1)
			}
			_, err := LoadBytes([]byte(data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want substring %q", err, tt.want)
			}
		})
	}
}
