package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"reasonix/internal/config"
)

type StoragePathView struct {
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	DefaultPath    string `json:"defaultPath"`
	SizeBytes      int64  `json:"sizeBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}

type StorageSettingsView struct {
	State            StoragePathView `json:"state"`
	Cache            StoragePathView `json:"cache"`
	Models           StoragePathView `json:"models"`
	Extensions       StoragePathView `json:"extensions"`
	DefaultWorkspace string          `json:"defaultWorkspace"`
	Platform         string          `json:"platform"`
	RestartRequired  bool            `json:"restartRequired"`
}

func storagePathView(kind string) StoragePathView {
	path := config.StoragePath(kind)
	defaultPath := ""
	switch kind {
	case "state":
		defaultPath = config.ReasonixHomeDir()
	case "cache":
		defaultPath = filepath.Join(config.ReasonixHomeDir(), "cache")
	case "models":
		defaultPath = filepath.Join(config.StoragePath("cache"), "models")
	case "extensions":
		defaultPath = filepath.Join(config.ReasonixHomeDir(), "plugins")
	}
	size, _ := storageTreeSize(path)
	available, _ := storageAvailableBytes(path)
	return StoragePathView{Kind: kind, Path: path, DefaultPath: defaultPath, SizeBytes: size, AvailableBytes: available}
}

func (a *App) StorageSettings() StorageSettingsView {
	return StorageSettingsView{
		State: storagePathView("state"), Cache: storagePathView("cache"), Models: storagePathView("models"), Extensions: storagePathView("extensions"),
		DefaultWorkspace: config.DefaultWorkspacePath(), Platform: runtime.GOOS,
		RestartRequired: a != nil && a.storageRestartRequired.Load(),
	}
}

func (a *App) SetDefaultWorkspace(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return config.UpdateStoragePreferences(func(prefs *config.StoragePreferences) error { prefs.DefaultWorkspace = ""; return nil })
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("default workspace is not a directory")
		}
		return err
	}
	return config.UpdateStoragePreferences(func(prefs *config.StoragePreferences) error { prefs.DefaultWorkspace = abs; return nil })
}

func (a *App) PickStorageFolder() (string, error) {
	if a == nil || a.ctx == nil {
		return "", errors.New("folder picker is unavailable")
	}
	return wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{Title: "Choose Reasonix storage folder"})
}

// MigrateStorage performs a verified copy and only then updates the bootstrap
// preference. The running process keeps its old roots; callers should restart
// after a successful migration.
func (a *App) MigrateStorage(kind, target string) (StorageSettingsView, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "state" && kind != "cache" && kind != "models" && kind != "extensions" {
		return StorageSettingsView{}, errors.New("unsupported storage category")
	}
	target, err := filepath.Abs(strings.TrimSpace(target))
	if err != nil || target == "" {
		return StorageSettingsView{}, errors.New("target directory is required")
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return StorageSettingsView{}, fmt.Errorf("target directory is not writable: %w", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return StorageSettingsView{}, fmt.Errorf("cannot inspect target directory: %w", err)
	}
	if len(entries) > 0 {
		return StorageSettingsView{}, errors.New("target directory must be empty")
	}
	source := config.StoragePath(kind)
	if source == "" {
		return StorageSettingsView{}, errors.New("current storage directory is unavailable")
	}
	sourceAbs, sourceErr := filepath.Abs(source)
	if sourceErr == nil {
		if sameDesktopPath(sourceAbs, target) {
			return StorageSettingsView{}, errors.New("target is the current storage directory")
		}
		if rel, relErr := filepath.Rel(sourceAbs, target); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return StorageSettingsView{}, errors.New("target cannot be inside the source directory")
		}
	}
	need, err := storageTreeSize(source)
	if os.IsNotExist(err) {
		need = 0
		err = nil
	}
	if err != nil {
		return StorageSettingsView{}, fmt.Errorf("cannot inspect source: %w", err)
	}
	available, err := storageAvailableBytes(target)
	if err != nil {
		return StorageSettingsView{}, fmt.Errorf("cannot inspect target disk: %w", err)
	}
	if need > available {
		return StorageSettingsView{}, fmt.Errorf("insufficient disk space: need %d bytes, have %d", need, available)
	}
	if need > 0 {
		if _, _, err := config.CopyStorageTree(source, target); err != nil {
			return StorageSettingsView{}, err
		}
	}
	prefs := config.LoadStoragePreferences()
	if err := config.UpdateStoragePreferences(func(p *config.StoragePreferences) error {
		switch kind {
		case "state":
			p.StatePath = target
		case "cache":
			p.CachePath = target
		case "models":
			p.ModelsPath = target
		case "extensions":
			p.ExtensionsPath = target
		}
		prefs = *p
		return nil
	}); err != nil {
		// The source remains authoritative until the bootstrap preference is
		// committed. Since targets are required to be empty, remove the staged
		// copy on this failure so a retry cannot mistake it for a completed move.
		_ = os.RemoveAll(target)
		return StorageSettingsView{}, err
	}
	if a != nil {
		a.storageRestartRequired.Store(true)
	}
	return StorageSettingsView{State: storagePathView("state"), Cache: storagePathView("cache"), Models: storagePathView("models"), Extensions: storagePathView("extensions"), DefaultWorkspace: prefs.DefaultWorkspace, Platform: runtime.GOOS, RestartRequired: true}, nil
}

func storageTreeSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}
