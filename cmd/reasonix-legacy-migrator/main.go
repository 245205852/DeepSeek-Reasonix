// Command reasonix-legacy-migrator is the one-shot flat→versioned install
// migrator used by v1.20+ packaging. In compatibility payloads it is still
// named reasonix-guard(.exe) so 1.18–1.19.1 updaters can hand off; the source
// and behavior are intentionally separate from the old Guard recovery product.
//
// It only: acquires a migration lock, validates the flat release unit, creates
// versions/<version>, writes current.json, rewrites entry points, starts the
// thin launcher, and self-deletes when possible. It never chooses safe mode,
// counts crashes, or auto-rolls back.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	"reasonix/internal/installlayout"
)

var version = "dev"

const migrationLockName = ".reasonix-layout-migrate.lock"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Println("reasonix-legacy-migrator", version)
			return 0
		case "help", "--help", "-h":
			fmt.Println("usage: reasonix-legacy-migrator [--install-root PATH] [--version VERSION]")
			return 0
		}
	}

	installRoot := ""
	activeVersion := strings.TrimSpace(version)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--install-root":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --install-root requires a path")
				return 2
			}
			i++
			installRoot = args[i]
		case "--version":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --version requires a value")
				return 2
			}
			i++
			activeVersion = args[i]
		case "launch", "--detach", "--safe-mode":
			// Accept legacy argv from old shortcuts; no product behavior.
			continue
		case "--app":
			if i+1 < len(args) {
				i++
			}
			continue
		default:
			if strings.HasPrefix(args[i], "--app=") {
				continue
			}
		}
	}
	if installRoot == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		installRoot = filepath.Dir(exe)
	}
	installRoot = filepath.Clean(installRoot)

	if err := migrate(installRoot, activeVersion); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func migrate(installRoot, activeVersion string) error {
	if err := installlayout.ValidateVersionName(activeVersion); err != nil {
		// Accept bare "dev" builds in development only.
		if activeVersion == "" || activeVersion == "dev" {
			activeVersion = "v0.0.0-dev"
			if err := installlayout.ValidateVersionName(activeVersion); err != nil {
				return err
			}
		} else if !strings.HasPrefix(activeVersion, "v") {
			activeVersion = "v" + activeVersion
			if err := installlayout.ValidateVersionName(activeVersion); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	unlock, err := acquireMigrationLock(installRoot)
	if err != nil {
		return err
	}
	defer unlock()

	// Pointer already committed: only cleanup, never overwrite the active version.
	if installlayout.HasCurrent(installRoot) {
		_ = cleanupLegacyFlatFiles(installRoot)
		_ = archiveLegacyRepairState(installRoot)
		_ = startLauncher(installRoot) // best-effort; layout already committed
		return nil
	}

	desktopName := installlayout.DesktopBinaryName()
	cliName := installlayout.CLIBinaryName()
	helperName := installlayout.UpdateHelperBinaryName()
	flatDesktop := filepath.Join(installRoot, desktopName)
	if _, err := os.Lstat(flatDesktop); err != nil {
		return fmt.Errorf("migrate: flat desktop binary missing: %w", err)
	}

	members := []installlayout.Member{
		{Name: desktopName, Path: flatDesktop},
	}
	if p := optionalRegular(filepath.Join(installRoot, cliName)); p != "" {
		members = append(members, installlayout.Member{Name: cliName, Path: p})
	} else {
		// CLI may be absent on some portable trees; synthesize from desktop only
		// is not allowed — require the whitelist. Prefer copying desktop as a
		// last-resort placeholder is forbidden; fail closed.
		return fmt.Errorf("migrate: flat CLI binary %s is required", cliName)
	}
	if p := optionalRegular(filepath.Join(installRoot, helperName)); p != "" {
		members = append(members, installlayout.Member{Name: helperName, Path: p})
	} else {
		return fmt.Errorf("migrate: flat update helper %s is required", helperName)
	}

	if err := installlayout.ActivateVersion(installlayout.ActivationRequest{
		InstallRoot: installRoot,
		Version:     activeVersion,
		RequestID:   "legacy-migrate",
		Members:     members,
	}); err != nil {
		return fmt.Errorf("migrate: activate versioned layout: %w", err)
	}

	if err := ensureLauncherEntry(installRoot); err != nil {
		return err
	}
	_ = cleanupLegacyFlatFiles(installRoot)
	_ = archiveLegacyRepairState(installRoot)
	// Launcher start is best-effort: layout activation is the commit point.
	if err := startLauncher(installRoot); err != nil {
		fmt.Fprintln(os.Stderr, "warning: start launcher after migrate:", err)
	}
	// Best-effort self-delete after the launcher is started.
	selfDelete()
	return nil
}

func optionalRegular(path string) string {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return path
}

func acquireMigrationLock(installRoot string) (func(), error) {
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(installRoot, migrationLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Stale lock from a dead migrator: if older than 10 minutes, steal it.
			info, statErr := os.Stat(path)
			if statErr == nil && time.Since(info.ModTime()) > 10*time.Minute {
				_ = os.Remove(path)
				f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("migrate: acquire lock: %w", err)
		}
	}
	_, _ = fmt.Fprintf(f, "pid=%d\nversion=%s\n", os.Getpid(), version)
	_ = f.Close()
	return func() { _ = os.Remove(path) }, nil
}

func ensureLauncherEntry(installRoot string) error {
	// Prefer an already-present thin launcher; otherwise leave a marker file so
	// packaging/installers can place one. During in-place migration from a flat
	// tree that only has reasonix-launcher.exe built from the old guard, keep it.
	launcher := "reasonix-launcher"
	if runtime.GOOS == "windows" {
		launcher += ".exe"
	}
	path := filepath.Join(installRoot, launcher)
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	// On Windows also accept Reasonix.exe as the alias.
	if runtime.GOOS == "windows" {
		if _, err := os.Lstat(filepath.Join(installRoot, "Reasonix.exe")); err == nil {
			return nil
		}
	}
	return fmt.Errorf("migrate: thin launcher %s is missing; install a signed package", launcher)
}

func cleanupLegacyFlatFiles(installRoot string) error {
	// Remove flat desktop/CLI/helper/guard sidecars after the version tree is
	// active. Never delete the thin launcher or current.json.
	names := []string{
		installlayout.DesktopBinaryName(),
		installlayout.CLIBinaryName(),
		installlayout.UpdateHelperBinaryName(),
	}
	if runtime.GOOS == "windows" {
		names = append(names, "reasonix-guard.exe")
	} else {
		names = append(names, "reasonix-guard")
	}
	for _, name := range names {
		path := filepath.Join(installRoot, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		_ = os.Remove(path)
	}
	return nil
}

func archiveLegacyRepairState(installRoot string) error {
	// Legacy repair state lives under the user Reasonix home, not install root.
	// Migrator only archives install-root markers; user-home archival is done by
	// the desktop on first successful versioned boot.
	markers := []string{
		"pending-update.json",
		"startup-state.json",
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	destRoot := filepath.Join(installRoot, "repair", "legacy-v1", ts)
	moved := false
	for _, name := range markers {
		src := filepath.Join(installRoot, name)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if err := os.MkdirAll(destRoot, 0o755); err != nil {
			return err
		}
		dest := filepath.Join(destRoot, name)
		if err := os.Rename(src, dest); err != nil {
			// Cross-device: copy+remove best effort.
			data, readErr := os.ReadFile(src)
			if readErr != nil {
				continue
			}
			if writeErr := fileutil.AtomicWriteFile(dest, data, 0o600); writeErr == nil {
				_ = os.Remove(src)
			}
		}
		moved = true
	}
	if moved {
		meta, _ := json.MarshalIndent(map[string]any{
			"migratedAt": time.Now().UTC().Format(time.RFC3339),
			"from":       "flat-v1",
			"to":         installlayout.InstallLayoutVersionedV1,
		}, "", "  ")
		_ = fileutil.AtomicWriteFile(filepath.Join(destRoot, "migration.json"), append(meta, '\n'), 0o644)
	}
	return nil
}

func startLauncher(installRoot string) error {
	launcher := "reasonix-launcher"
	if runtime.GOOS == "windows" {
		launcher += ".exe"
	}
	path := filepath.Join(installRoot, launcher)
	if _, err := os.Lstat(path); err != nil {
		if runtime.GOOS == "windows" {
			path = filepath.Join(installRoot, "Reasonix.exe")
		}
	}
	if _, err := os.Lstat(path); err != nil {
		// Migration succeeded; user can start manually.
		return nil
	}
	cmd := exec.Command(path)
	cmd.Dir = installRoot
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if runtime.GOOS == "windows" {
		return cmd.Start()
	}
	return cmd.Start()
}

func selfDelete() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	// Do not delete if we are not named like a compatibility guard payload.
	base := strings.ToLower(filepath.Base(exe))
	if base != "reasonix-guard" && base != "reasonix-guard.exe" {
		return
	}
	_ = os.Remove(exe)
}
