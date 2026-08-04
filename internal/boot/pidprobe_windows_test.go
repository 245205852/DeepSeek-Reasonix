//go:build windows

package boot

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether pid still exists. Windows has no signal-0 probe, so
// open a query-limited handle: it fails when the process is gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		_ = syscall.CloseHandle(h)
		return true
	}
	_ = syscall.CloseHandle(h)
	return code == 259 // STILL_ACTIVE
}
