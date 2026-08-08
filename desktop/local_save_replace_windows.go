//go:build windows

package main

import "os"

// Windows does not replace an existing file with os.Rename. Remove the old
// directory entry first, then rename the completed temporary file into place;
// removing a hard-link or symlink alias never truncates its target.
func replaceLocalSaveDestination(temp, target string) error {
	if err := os.Rename(temp, target); err == nil {
		return nil
	} else if _, statErr := os.Lstat(target); statErr != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	return os.Rename(temp, target)
}
