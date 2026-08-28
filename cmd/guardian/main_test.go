package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mihomo-guardian/internal/config"
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
