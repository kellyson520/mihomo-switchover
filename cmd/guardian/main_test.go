package main

import "testing"

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
