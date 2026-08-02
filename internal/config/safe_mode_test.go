package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSafeModeRequestedAlwaysFalse(t *testing.T) {
	t.Setenv("REASONIX_SAFE_MODE", "1")
	if SafeModeRequested() {
		t.Fatal("v1.20+ must ignore REASONIX_SAFE_MODE")
	}
}

func TestBuiltinDefaultsDoNotReadOrRewriteMalformedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	path := filepath.Join(home, "config.toml")
	bad := []byte("[broken\n")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := LoadBuiltinDefaultsForRoot(t.TempDir())
	if cfg == nil || cfg.SafeMode() || len(cfg.Providers) == 0 {
		t.Fatalf("builtin defaults = %+v (safeMode must be false)", cfg)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bad) {
		t.Fatalf("malformed config was rewritten: %q", got)
	}
}

func TestRecoveryDefaultsAliasBuiltin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	cfg := LoadRecoveryDefaultsForRoot(t.TempDir())
	if cfg == nil || cfg.SafeMode() {
		t.Fatalf("recovery defaults must not set SafeMode: %+v", cfg)
	}
}
