package main

import (
	"os"
	"path/filepath"
	"testing"

	"reasonix/internal/installlayout"
)

func TestMigrateFlatInstallToVersioned(t *testing.T) {
	root := t.TempDir()
	// Flat release unit.
	for _, name := range installlayout.AllowedVersionMembers() {
		if err := os.WriteFile(filepath.Join(root, name), []byte("flat-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Thin launcher entry must already exist (packaging places it). Use a
	// non-executable marker so startLauncher fails closed without hanging.
	_ = os.WriteFile(filepath.Join(root, "reasonix-launcher"), []byte("launcher"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "reasonix-launcher.exe"), []byte("launcher"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "Reasonix.exe"), []byte("launcher"), 0o644)
	// Marker files that must be archived, not deleted silently without record.
	_ = os.WriteFile(filepath.Join(root, "pending-update.json"), []byte(`{"pending":true}`), 0o644)
	_ = os.WriteFile(filepath.Join(root, "startup-state.json"), []byte(`{}`), 0o644)

	if err := migrate(root, "v1.20.0"); err != nil {
		t.Fatal(err)
	}
	if !installlayout.HasCurrent(root) {
		t.Fatal("current.json missing after migration")
	}
	ptr, err := installlayout.ReadCurrent(root)
	if err != nil || ptr.ActiveVersion != "v1.20.0" {
		t.Fatalf("pointer=%+v err=%v", ptr, err)
	}
	// Flat desktop must be cleaned up after successful activation.
	if _, err := os.Stat(filepath.Join(root, installlayout.DesktopBinaryName())); !os.IsNotExist(err) {
		t.Fatal("flat desktop should be removed after migration")
	}
	// Active desktop lives under versions/.
	if _, err := installlayout.ActiveDesktopPath(root); err != nil {
		t.Fatal(err)
	}
	// Second run is idempotent cleanup-only.
	if err := migrate(root, "v1.20.0"); err != nil {
		t.Fatalf("idempotent re-run: %v", err)
	}
	ptr2, err := installlayout.ReadCurrent(root)
	if err != nil || ptr2.ActiveVersion != "v1.20.0" {
		t.Fatalf("re-run overwrote active version: %+v", ptr2)
	}
}

func TestMigrateRefusesWithoutFlatUnit(t *testing.T) {
	root := t.TempDir()
	if err := migrate(root, "v1.20.0"); err == nil {
		t.Fatal("expected failure without flat desktop")
	}
}

func TestMigrateRefusesCorruptCurrentPointerWithoutOverwritingIt(t *testing.T) {
	root := t.TempDir()
	corrupt := []byte(`{"schemaVersion":99}`)
	current := filepath.Join(root, installlayout.CurrentFileName)
	if err := os.WriteFile(current, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range installlayout.AllowedVersionMembers() {
		if err := os.WriteFile(filepath.Join(root, name), []byte("stale-"+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(root, "v1.20.0"); err == nil {
		t.Fatal("corrupt current.json was treated as an absent pointer")
	}
	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("corrupt pointer was overwritten: %q", got)
	}
}
