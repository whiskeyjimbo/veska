//go:build !linux && !darwin

package loader

// ReadRSSBytes returns 0 on unsupported platforms.
func ReadRSSBytes() int64 { return 0 }
