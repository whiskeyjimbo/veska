// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux && !darwin

package loader

// ReadRSSBytes returns 0 on unsupported platforms.
func ReadRSSBytes() int64 { return 0 }
