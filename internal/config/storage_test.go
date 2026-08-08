package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoragePreferencesRoundTripAndEnvironmentOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REASONIX_HOME", home)
	t.Setenv("REASONIX_STATE_HOME", "")
	t.Setenv("REASONIX_CACHE_HOME", "")
	prefs := StoragePreferences{StatePath: filepath.Join(home, "data"), CachePath: filepath.Join(home, "cache2"), ModelsPath: filepath.Join(home, "models"), ExtensionsPath: filepath.Join(home, "extensions"), DefaultWorkspace: filepath.Join(home, "workspace")}
	if err := SaveStoragePreferences(prefs); err != nil {
		t.Fatal(err)
	}
	got := LoadStoragePreferences()
	if got != prefs {
		t.Fatalf("preferences = %#v, want %#v", got, prefs)
	}
	if StoragePath("state") != prefs.StatePath || StoragePath("cache") != prefs.CachePath {
		t.Fatalf("configured paths not resolved: %#v", got)
	}
	t.Setenv("REASONIX_STATE_HOME", filepath.Join(home, "env-state"))
	if StoragePath("state") != filepath.Join(home, "env-state") {
		t.Fatal("environment override did not win")
	}
}

func TestCopyStorageTreeVerifiesAndKeepsBootstrapFilesOut(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(filepath.Join(src, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.toml": "config", ".env": "secret", "storage.json": "bootstrap", "sessions/chat.jsonl": "hello"} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bytes, files, err := CopyStorageTree(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if bytes != int64(len("hello")) || files != 1 {
		t.Fatalf("copy totals = %d/%d", bytes, files)
	}
	if _, err := os.Stat(filepath.Join(dst, "sessions", "chat.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.toml", ".env", "storage.json"} {
		if _, err := os.Stat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Fatalf("bootstrap file copied: %s", name)
		}
	}
	if _, _, err := CopyStorageTree(src, filepath.Join(src, "nested")); err == nil {
		t.Fatal("nested target should be rejected")
	}
}
