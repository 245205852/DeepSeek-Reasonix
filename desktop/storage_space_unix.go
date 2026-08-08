//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
	"path/filepath"
)

func storageAvailableBytes(path string) (int64, error) {
	path = filepath.Clean(path)
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		parent := filepath.Dir(path)
		if err := unix.Statfs(parent, &stat); err != nil {
			return 0, err
		}
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
