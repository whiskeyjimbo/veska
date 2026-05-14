//go:build !linux

package repo

// CurrentRSS is a no-op on non-Linux platforms; returns 0, nil.
func CurrentRSS() (int64, error) {
	return 0, nil
}
