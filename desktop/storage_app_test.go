package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/config"
)

func TestSetDefaultWorkspacePersistsBootstrapPreference(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_STATE_HOME", "")
	t.Setenv("REASONIX_CACHE_HOME", "")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&App{}).SetDefaultWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if got := config.DefaultWorkspacePath(); got != workspace {
		t.Fatalf("default workspace = %q, want %q", got, workspace)
	}
}

func TestMigrateStorageRejectsNonEmptyTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_STATE_HOME", "")
	t.Setenv("REASONIX_CACHE_HOME", "")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "existing"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&App{}).MigrateStorage("state", target); err == nil {
		t.Fatal("expected non-empty target rejection")
	}
}
