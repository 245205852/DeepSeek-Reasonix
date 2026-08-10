//go:build !darwin && !linux && !windows

package sessioncatalog

func catalogFilesystemRemote(string) bool { return false }
