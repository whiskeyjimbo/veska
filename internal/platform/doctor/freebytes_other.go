//go:build !linux

package doctor

// getFreeBytes returns 0 on non-Linux platforms.
// Free-space reporting is not implemented outside Linux.
func getFreeBytes(_ string) int64 {
	return 0
}
