//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepairExistingWindowsShortcutsRepairsOnlyExistingFiles(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "Reasonix.lnk")
	missing := filepath.Join(root, "Missing.lnk")
	if err := os.WriteFile(existing, []byte("old shortcut"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, "reasonix-launcher.exe")
	var wrote, notified []string
	originalNotify := windowsNotifyShortcutChange
	windowsNotifyShortcutChange = func(path string) { notified = append(notified, path) }
	t.Cleanup(func() { windowsNotifyShortcutChange = originalNotify })

	err := repairExistingWindowsShortcuts(
		[]string{existing, missing},
		launcher,
		func(path, target string) error {
			if target != launcher {
				t.Fatalf("shortcut target = %q, want %q", target, launcher)
			}
			wrote = append(wrote, path)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrote) != 1 || wrote[0] != existing {
		t.Fatalf("rewritten shortcuts = %v, want only %q", wrote, existing)
	}
	if len(notified) != 1 || notified[0] != existing {
		t.Fatalf("shell notifications = %v, want only %q", notified, existing)
	}
}
