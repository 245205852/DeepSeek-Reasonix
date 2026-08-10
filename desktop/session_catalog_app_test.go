package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectTreeSnapshotReturnsProjectShellWithoutMigratingSessions(t *testing.T) {
	isolateDesktopUserDirs(t)
	root := t.TempDir()
	if err := addProject(root, "Large Project"); err != nil {
		t.Fatal(err)
	}
	sessionDir := desktopSessionDir(root)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(sessionDir, "legacy.jsonl")
	if err := os.WriteFile(legacyPath, []byte(`{"role":"user","content":"legacy"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := NewApp().GetProjectTreeSnapshot()
	if len(snapshot.Projects) != 1 || snapshot.Projects[0].Root != root {
		t.Fatalf("snapshot = %#v, want project shell %q", snapshot, root)
	}
	if snapshot.Projects[0].Children == nil {
		t.Fatal("project shell children encoded as null, want []")
	}
	if _, err := os.Stat(legacyPath + ".meta"); !os.IsNotExist(err) {
		t.Fatalf("snapshot migrated session metadata: %v", err)
	}
}

func TestCompatibilityProjectTreeDoesNotMigrateLegacySession(t *testing.T) {
	isolateDesktopUserDirs(t)
	root := t.TempDir()
	if err := addProject(root, "Project"); err != nil {
		t.Fatal(err)
	}
	sessionDir := desktopSessionDir(root)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(sessionDir, "legacy.jsonl")
	if err := os.WriteFile(legacyPath, []byte(`{"role":"user","content":"legacy"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = NewApp().ListProjectTree()
	if _, err := os.Stat(legacyPath + ".meta"); !os.IsNotExist(err) {
		t.Fatalf("ListProjectTree migrated legacy session: %v", err)
	}
}
