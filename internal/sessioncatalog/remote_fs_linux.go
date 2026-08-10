//go:build linux

package sessioncatalog

import "syscall"

const (
	nfsSuperMagic    = 0x6969
	smbSuperMagic    = 0x517B
	cifsSuperMagic   = 0xFF534D42
	nfsdSuperMagic   = 0x6E667364
	cephSuperMagic   = 0x00C36400
	lustreSuperMagic = 0x0BD00BD0
)

func catalogFilesystemRemote(path string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return false
	}
	switch uint64(stat.Type) {
	case nfsSuperMagic, smbSuperMagic, cifsSuperMagic, nfsdSuperMagic, cephSuperMagic, lustreSuperMagic:
		return true
	default:
		return false
	}
}
