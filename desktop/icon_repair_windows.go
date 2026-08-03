//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"

	"reasonix/internal/installlayout"
)

var (
	windowsKnownFolderPath      = windows.KnownFolderPath
	windowsWriteShortcut        = writeWindowsShortcut
	windowsNotifyShortcutChange = notifyWindowsShortcutChanged
)

func repairDesktopIconIntegration() error {
	executable, err := os.Executable()
	if err != nil {
		return nil
	}
	installRoot, err := installlayout.ResolveInstallRoot(executable)
	if err != nil || installRoot == "" {
		return nil
	}
	launcher := filepath.Join(installRoot, "reasonix-launcher.exe")
	if info, err := os.Lstat(launcher); err != nil || !info.Mode().IsRegular() {
		return nil
	}
	paths, err := reasonixWindowsShortcutPaths()
	if err != nil {
		return err
	}
	return repairExistingWindowsShortcuts(paths, launcher, windowsWriteShortcut)
}

func reasonixWindowsShortcutPaths() ([]string, error) {
	desktop, desktopErr := windowsKnownFolderPath(windows.FOLDERID_Desktop, windows.KF_FLAG_DEFAULT)
	programs, programsErr := windowsKnownFolderPath(windows.FOLDERID_Programs, windows.KF_FLAG_DEFAULT)
	if desktopErr != nil || programsErr != nil {
		return nil, errors.Join(desktopErr, programsErr)
	}
	return []string{
		filepath.Join(desktop, "Reasonix.lnk"),
		filepath.Join(programs, "Reasonix.lnk"),
	}, nil
}

func repairExistingWindowsShortcuts(paths []string, launcher string, writer func(string, string) error) error {
	var repairErr error
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				repairErr = errors.Join(repairErr, err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := writer(path, launcher); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("%s: %w", path, err))
			continue
		}
		windowsNotifyShortcutChange(path)
	}
	return repairErr
}

func writeWindowsShortcut(shortcutPath, launcher string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()

	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer shell.Release()
	created, err := oleutil.CallMethod(shell, "CreateShortcut", shortcutPath)
	if err != nil {
		return err
	}
	shortcut := created.ToIDispatch()
	if shortcut == nil {
		_ = created.Clear()
		return fmt.Errorf("WScript.Shell returned no shortcut object")
	}
	defer shortcut.Release()

	for name, value := range map[string]string{
		"TargetPath":       launcher,
		"IconLocation":     launcher + ",0",
		"WorkingDirectory": filepath.Dir(launcher),
		"Description":      "Reasonix",
	} {
		result, err := oleutil.PutProperty(shortcut, name, value)
		if result != nil {
			_ = result.Clear()
		}
		if err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
	}
	result, err := oleutil.CallMethod(shortcut, "Save")
	if result != nil {
		_ = result.Clear()
	}
	return err
}

func notifyWindowsShortcutChanged(path string) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	// SHCNE_UPDATEITEM + SHCNF_PATHW asks Explorer to invalidate only this
	// shortcut instead of flushing the entire association cache.
	const (
		shcneUpdateItem = 0x00002000
		shcnfPathW      = 0x0005
	)
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("SHChangeNotify")
	_, _, _ = proc.Call(shcneUpdateItem, shcnfPathW, uintptr(unsafe.Pointer(pathPtr)), 0)
}
