package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mihomo-guardian/internal/config"
	"mihomo-guardian/internal/mihomo"
	"mihomo-guardian/internal/quality"
	guardianruntime "mihomo-guardian/internal/runtime"
)

func TestParseRunCommandRequiresConfig(t *testing.T) {
	if _, err := parseRunArgs([]string{"run"}); err == nil {
		t.Fatal("expected missing config error")
	}
}

func TestParseRunCommandKeepsMihomoDefaults(t *testing.T) {
	args, err := parseRunArgs([]string{"run", "--config", "/guardian/guardian.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if args.MihomoPath != "/mihomo" || args.MihomoConfigDir != "/root/.config/mihomo" {
		t.Fatalf("args=%+v", args)
	}
}

func TestParseCommandAcceptsStatusWithConfig(t *testing.T) {
	args, rest, err := parseCommandArgs("status", []string{"--config", "/guardian/guardian.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if args.ConfigPath != "/guardian/guardian.yaml" || len(rest) != 0 {
		t.Fatalf("args=%+v rest=%v", args, rest)
	}
}

func TestParseSwitchCommandKeepsTargetArgument(t *testing.T) {
	args, rest, err := parseCommandArgs("switch", []string{"--config", "/guardian/guardian.yaml", "backup"})
	if err != nil {
		t.Fatal(err)
	}
	if args.ConfigPath != "/guardian/guardian.yaml" || len(rest) != 1 || rest[0] != "backup" {
		t.Fatalf("args=%+v rest=%v", args, rest)
	}
}

func TestParseQualityDaemonRequiresPersistentExecutionPaths(t *testing.T) {
	args, err := parseQualityDaemonArgs([]string{
		"--config", "/persistent/guardian.yaml",
		"--data", "/persistent/data",
		"--logs", "/persistent/logs",
		"--secret-file", "/persistent/controller_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.ConfigPath != "/persistent/guardian.yaml" || args.DataDir != "/persistent/data" ||
		args.LogsDir != "/persistent/logs" || args.SecretFile != "/persistent/controller_secret" {
		t.Fatalf("quality daemon args=%+v", args)
	}
	if _, err := parseQualityDaemonArgs([]string{"--config", "/persistent/guardian.yaml"}); err == nil {
		t.Fatal("quality daemon accepted missing persistent paths")
	}
}

func TestParseQualityRunKeepsOptionalTargetAndPersistentPaths(t *testing.T) {
	args, err := parseQualityRunArgs([]string{
		"--target", "reserve",
		"--secret-file", "/run/secret",
		"--logs", "/persistent/logs",
		"--data", "/persistent/data",
		"--config", "/persistent/guardian.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.Target != "reserve" || args.ConfigPath != "/persistent/guardian.yaml" ||
		args.DataDir != "/persistent/data" || args.LogsDir != "/persistent/logs" || args.SecretFile != "/run/secret" {
		t.Fatalf("quality run args=%+v", args)
	}
}

func TestParseQualityStatusDoesNotRequireSecretOrLogs(t *testing.T) {
	args, err := parseQualityStatusArgs([]string{
		"--data", "/persistent/data",
		"--config", "/persistent/guardian.yaml",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.ConfigPath != "/persistent/guardian.yaml" || args.DataDir != "/persistent/data" {
		t.Fatalf("quality status args=%+v", args)
	}
}

func TestParseQualityBaselineResetRequiresExactIdentity(t *testing.T) {
	args, err := parseQualityBaselineResetArgs([]string{
		"--config", "/persistent/guardian.yaml",
		"--data", "/persistent/data",
		"--target", "primary",
		"--node", "node-a",
		"--ip", "198.51.100.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.ConfigPath != "/persistent/guardian.yaml" || args.DataDir != "/persistent/data" ||
		args.Target != "primary" || args.Node != "node-a" || args.IP != "198.51.100.10" {
		t.Fatalf("quality baseline reset args=%+v", args)
	}
	if _, err := parseQualityBaselineResetArgs([]string{
		"--config", "/persistent/guardian.yaml", "--data", "/persistent/data",
		"--target", "primary", "--node", "node-a",
	}); err == nil {
		t.Fatal("baseline reset accepted missing exact IP identity")
	}
}

func TestResetQualityBaselineAuditsExactIdentityAndKeepsHistory(t *testing.T) {
	reports := quality.NewStore(t.TempDir())
	key := quality.NodeKey{Target: "primary", Provider: "provider", Node: "node-a", IPFamily: "ipv4", IP: "198.51.100.10"}
	firstAt := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	first := quality.Report{Identity: key, ObservedAt: firstAt, QualityScore: 80, StabilityScore: 80, EffectiveScore: 80, ConfidencePercent: 90, Complete: true}
	second := first
	second.ObservedAt = firstAt.Add(time.Hour)
	second.QualityScore = 60
	second.StabilityScore = 60
	second.EffectiveScore = 60
	if _, err := reports.SaveReport(first); err != nil {
		t.Fatal(err)
	}
	if _, err := reports.SaveReport(second); err != nil {
		t.Fatal(err)
	}

	oldScore, newScore, err := resetQualityBaseline(reports, key, firstAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if oldScore != 80 || newScore != 60 {
		t.Fatalf("baseline scores=%d/%d, want 80/60", oldScore, newScore)
	}
	record, err := reports.LoadNode(key)
	if err != nil {
		t.Fatal(err)
	}
	if record.Baseline == nil || record.Baseline.Score != 60 {
		t.Fatalf("baseline=%+v, want reset to latest score", record.Baseline)
	}
	history, err := reports.ListReports(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history entries=%d, want both reports retained", len(history))
	}
	auditData, err := os.ReadFile(filepath.Join(reports.Root(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(auditData))), &audit); err != nil {
		t.Fatal(err)
	}
	if audit["event"] != "baseline_reset" {
		t.Fatalf("audit=%v", audit)
	}
	identity, ok := audit["identity"].(map[string]any)
	if !ok || identity["target"] != "primary" || identity["node"] != "node-a" || identity["ip"] != "198.51.100.10" {
		t.Fatalf("audit identity=%v", audit["identity"])
	}
}

type qualityLinkTestAPI struct{}

func (qualityLinkTestAPI) Heartbeat(context.Context) error { return errors.New("connection refused") }

func (qualityLinkTestAPI) GetProxy(context.Context, string) (mihomo.Proxy, error) {
	return mihomo.Proxy{}, errors.New("must not read proxy after link loss")
}

func (qualityLinkTestAPI) GetProvider(context.Context, string) (mihomo.Provider, error) {
	return mihomo.Provider{}, errors.New("must not read provider after link loss")
}

func (qualityLinkTestAPI) SetProxy(context.Context, string, string) error {
	return errors.New("must not select a node after link loss")
}

func TestRunQualityScanReturnsQualityLinkWithoutSelectingOrProbing(t *testing.T) {
	reports := quality.NewStore(t.TempDir())
	cfg := config.Config{Quality: config.QualityConfig{Enabled: true}}
	err := runQualityScan(context.Background(), cfg, qualityLinkTestAPI{}, reports, nil, nil, "")
	if !errors.Is(err, quality.ErrQualityLink) {
		t.Fatalf("quality scan error=%v, want quality-link error", err)
	}
}

func TestServiceLinkRejectsInfrastructureChangesDuringHotReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guardian.yaml")
	content := []byte(`
mihomo:
  api: http://127.0.0.1:9090
  proxy: http://127.0.0.1:7890
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
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	loader := config.NewLoader(path)
	current, err := loader.Load()
	if err != nil {
		t.Fatal(err)
	}
	link := &serviceLink{
		loader: loader, cfg: current,
		service: guardianruntime.NewService(current, nil, nil, nil, nil),
	}
	changed := strings.Replace(string(content), "7890", "7891", 1) + "\n"
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := link.Reload(); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("infrastructure reload was accepted: %v", err)
	}
	if link.cfg.Mihomo.Proxy != "http://127.0.0.1:7890" {
		t.Fatalf("running proxy configuration changed: %s", link.cfg.Mihomo.Proxy)
	}
}
