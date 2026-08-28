package config

import (
	"os"
	"path/filepath"
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
