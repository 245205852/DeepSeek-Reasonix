// Command reasonix-launcher is the permanent thin desktop entry point for the
// versioned install layout (v1.20+). It reads InstallRoot/current.json, starts
// the active reasonix-desktop binary, and exits. It never counts crashes,
// chooses previous versions, or enters a product safe mode.
//
// Migration compatibility: old shortcuts may still pass "launch", "--detach",
// or "--safe-mode". Those tokens are stripped and ignored.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"reasonix/internal/installlayout"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Println("reasonix-launcher", version)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		}
	}

	installRoot, err := resolveInstallRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	desktopPath, err := resolveDesktopPath(installRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	childArgs := stripLegacyLaunchArgs(args)
	cmd := exec.Command(desktopPath, childArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir = installRoot

	// Windows packaged entry points start the desktop and exit so the launcher
	// never becomes a long-lived parent that would own crash accounting.
	if detachByDefault() {
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func resolveInstallRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve launcher path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		// Fall back to the raw path when EvalSymlinks is unavailable.
		exe, _ = os.Executable()
	}
	return filepath.Clean(filepath.Dir(exe)), nil
}

func resolveDesktopPath(installRoot string) (string, error) {
	if path, err := installlayout.ActiveDesktopPath(installRoot); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) && !isMissingCurrent(err) {
		// Invalid pointer is fatal: never fall back to a random sibling when a
		// corrupted current.json is present.
		if installlayout.HasCurrent(installRoot) {
			return "", err
		}
		// Missing current.json: allow flat-layout sibling during migration.
		if path := siblingDesktop(installRoot); path != "" {
			return path, nil
		}
		return "", err
	}
	if path := siblingDesktop(installRoot); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("cannot locate reasonix-desktop under %s (missing current.json)", installRoot)
}

func isMissingCurrent(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return os.IsNotExist(err) ||
		strings.Contains(msg, "current.json") ||
		strings.Contains(msg, "no such file")
}

func siblingDesktop(installRoot string) string {
	name := installlayout.DesktopBinaryName()
	path := filepath.Join(installRoot, name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return path
}

// stripLegacyLaunchArgs removes tokens old shortcuts may still send:
//   launch --detach --safe-mode --app PATH
// The thin launcher never honors safe mode or detach flags as product behavior;
// Windows already detaches by default when this binary is the entry point.
func stripLegacyLaunchArgs(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for i := 0; i < len(args); i++ {
		if skipNext {
			skipNext = false
			continue
		}
		arg := args[i]
		switch arg {
		case "launch", "--detach", "--safe-mode", "-safe-mode":
			continue
		case "--app":
			// Drop --app and its value; the versioned layout owns discovery.
			skipNext = true
			continue
		case "--":
			// Preserve a deliberate separator and the remainder unchanged.
			out = append(out, args[i:]...)
			return out
		}
		if strings.HasPrefix(arg, "--app=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func detachByDefault() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	name := strings.ToLower(filepath.Base(exe))
	return name == "reasonix-launcher.exe" || name == "reasonix.exe"
}

func usage() {
	fmt.Println("usage: reasonix-launcher [args...]")
	fmt.Println("  Starts the active Reasonix desktop from current.json.")
	fmt.Println("  Legacy --safe-mode / launch --detach tokens are ignored.")
}
