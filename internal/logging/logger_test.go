package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerRedactsSecrets(t *testing.T) {
	var b bytes.Buffer
	logger := NewLogger(&b, LoggerConfig{MaxBytes: 1 << 20, Retain: 2})
	if err := logger.Event("probe", map[string]any{
		"url":    "https://x.test?a=token-secret&key=api-secret",
		"secret": "controller-secret",
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, forbidden := range []string{"token-secret", "api-secret", "controller-secret"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("secret leaked: %s", forbidden)
		}
	}
	if !strings.Contains(out, `"event":"probe"`) {
		t.Fatalf("missing event: %s", out)
	}
}

func TestFileLoggerRotatesBeforeExceedingLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guardian.jsonl")
	logger, err := NewFileLogger(path, LoggerConfig{MaxBytes: 220, Retain: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := logger.Event("large", map[string]any{"payload": strings.Repeat("x", 80)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation file missing: %v", err)
	}
}
