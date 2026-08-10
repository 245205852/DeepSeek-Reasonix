//go:build windows

package sessioncatalog

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func catalogFilesystemRemote(path string) bool {
	volume := filepath.VolumeName(filepath.Clean(path))
	if volume == "" {
		return false
	}
	root := volume
	if !strings.HasSuffix(root, `\`) {
		root += `\`
	}
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	return windows.GetDriveType(rootPtr) == windows.DRIVE_REMOTE
}
