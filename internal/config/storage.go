package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// StoragePreferences is deliberately kept outside config.toml. The file is
// read before the rest of the application starts so a relocated state root
// cannot make the bootstrap configuration unreachable.
type StoragePreferences struct {
	StatePath        string `json:"statePath,omitempty"`
	CachePath        string `json:"cachePath,omitempty"`
	ModelsPath       string `json:"modelsPath,omitempty"`
	ExtensionsPath   string `json:"extensionsPath,omitempty"`
	DefaultWorkspace string `json:"defaultWorkspace,omitempty"`
}

var storagePrefsMu sync.Mutex

func StoragePreferencesPath() string {
	home := reasonixHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "storage.json")
}

func LoadStoragePreferences() StoragePreferences {
	path := StoragePreferencesPath()
	if path == "" {
		return StoragePreferences{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return StoragePreferences{}
	}
	var prefs StoragePreferences
	if json.Unmarshal(b, &prefs) != nil {
		return StoragePreferences{}
	}
	prefs.StatePath = cleanStoragePath(prefs.StatePath)
	prefs.CachePath = cleanStoragePath(prefs.CachePath)
	prefs.ModelsPath = cleanStoragePath(prefs.ModelsPath)
	prefs.ExtensionsPath = cleanStoragePath(prefs.ExtensionsPath)
	prefs.DefaultWorkspace = cleanStoragePath(prefs.DefaultWorkspace)
	return prefs
}

func SaveStoragePreferences(prefs StoragePreferences) error {
	path := StoragePreferencesPath()
	if path == "" {
		return errors.New("cannot resolve Reasonix bootstrap directory")
	}
	prefs.StatePath = cleanStoragePath(prefs.StatePath)
	prefs.CachePath = cleanStoragePath(prefs.CachePath)
	prefs.ModelsPath = cleanStoragePath(prefs.ModelsPath)
	prefs.ExtensionsPath = cleanStoragePath(prefs.ExtensionsPath)
	prefs.DefaultWorkspace = cleanStoragePath(prefs.DefaultWorkspace)
	b, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	storagePrefsMu.Lock()
	defer storagePrefsMu.Unlock()
	return saveStoragePreferencesLocked(path, prefs, b)
}

// UpdateStoragePreferences serializes a read-modify-write cycle so concurrent
// settings calls cannot lose a path change made by another window.
func UpdateStoragePreferences(mutate func(*StoragePreferences) error) error {
	path := StoragePreferencesPath()
	if path == "" {
		return errors.New("cannot resolve Reasonix bootstrap directory")
	}
	storagePrefsMu.Lock()
	defer storagePrefsMu.Unlock()
	prefs := StoragePreferences{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &prefs)
	}
	if err := mutate(&prefs); err != nil {
		return err
	}
	prefs.StatePath = cleanStoragePath(prefs.StatePath)
	prefs.CachePath = cleanStoragePath(prefs.CachePath)
	prefs.ModelsPath = cleanStoragePath(prefs.ModelsPath)
	prefs.ExtensionsPath = cleanStoragePath(prefs.ExtensionsPath)
	prefs.DefaultWorkspace = cleanStoragePath(prefs.DefaultWorkspace)
	b, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	return saveStoragePreferencesLocked(path, prefs, b)
}

func saveStoragePreferencesLocked(path string, _ StoragePreferences, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cleanStoragePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = ExpandVars(path)
	if path == "~" {
		if home, err := osUserHomeDir(); err == nil {
			path = home
		}
	} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := osUserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return filepath.Clean(path)
}

// StoragePath resolves a user-selectable storage category. Environment
// variables remain an explicit override for existing CLI/CI deployments.
func StoragePath(kind string) string {
	prefs := LoadStoragePreferences()
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "state":
		if env := cleanEnvDir("REASONIX_STATE_HOME"); env != "" {
			return env
		}
		if prefs.StatePath != "" {
			return prefs.StatePath
		}
		return reasonixHomeDir()
	case "cache":
		if env := cleanEnvDir("REASONIX_CACHE_HOME"); env != "" {
			return env
		}
		if prefs.CachePath != "" {
			return prefs.CachePath
		}
		return defaultUserCacheDir()
	case "models":
		if prefs.ModelsPath != "" {
			return prefs.ModelsPath
		}
		return filepath.Join(StoragePath("cache"), "models")
	case "extensions":
		if prefs.ExtensionsPath != "" {
			return prefs.ExtensionsPath
		}
		return filepath.Join(reasonixHomeDir(), "plugins")
	default:
		return ""
	}
}

func defaultUserCacheDir() string {
	if dir := cleanEnvDir("REASONIX_HOME"); dir != "" {
		return filepath.Join(dir, "cache")
	}
	if dir := osUserCacheDir(); dir != "" {
		return filepath.Join(dir, "reasonix")
	}
	return ""
}

func DefaultWorkspacePath() string { return LoadStoragePreferences().DefaultWorkspace }

// CopyStorageTree copies a directory to a temporary sibling and verifies the
// resulting file count and byte total before the caller switches preferences.
// It intentionally skips bootstrap files that must remain in the fixed home.
func CopyStorageTree(src, target string) (int64, int, error) {
	src = filepath.Clean(strings.TrimSpace(src))
	target = filepath.Clean(strings.TrimSpace(target))
	if src == "" || target == "" {
		return 0, 0, errors.New("source and target are required")
	}
	if samePath(src, target) {
		return 0, 0, errors.New("target is the current storage directory")
	}
	rel, err := filepath.Rel(src, target)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, 0, errors.New("target cannot be inside the source directory")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, 0, err
	}
	tmp := target + ".reasonix-migrating"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return 0, 0, err
	}
	var bytes int64
	var files int
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == "config.toml" || rel == ".env" || rel == "storage.json" || rel == "storage.json.tmp" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		dst := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, dst); err != nil {
				return err
			}
			return nil
		}
		if err := storageCopyFile(path, dst, info.Mode().Perm()); err != nil {
			return err
		}
		bytes += info.Size()
		files++
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return 0, 0, err
	}
	var checkBytes int64
	var checkFiles int
	_ = filepath.WalkDir(tmp, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			checkBytes += info.Size()
			checkFiles++
		}
		return nil
	})
	if checkBytes != bytes || checkFiles != files {
		_ = os.RemoveAll(tmp)
		return 0, 0, fmt.Errorf("migration verification failed: copied %d/%d files and %d/%d bytes", checkFiles, files, checkBytes, bytes)
	}
	_ = os.RemoveAll(target)
	if err := os.Rename(tmp, target); err != nil {
		_ = os.RemoveAll(tmp)
		return 0, 0, err
	}
	return bytes, files, nil
}

func storageCopyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
