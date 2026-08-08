//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"path/filepath"
)

func storageAvailableBytes(path string) (int64, error) {
	path = filepath.Clean(path)
	var free, total, avail uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &free, &total, &avail); err != nil {
		if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(filepath.Dir(path)), &free, &total, &avail); err != nil {
			return 0, err
		}
	}
	return int64(avail), nil
}
