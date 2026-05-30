//go:build linux

package doctor

import "syscall"

// getFreeBytes returns the number of free bytes available on the filesystem
// containing path.  Uses syscall.Statfs (stdlib, no CGo).
func getFreeBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	// Bavail = blocks available to unprivileged users; Bsize = block size.
	return int64(stat.Bavail) * stat.Bsize
}
