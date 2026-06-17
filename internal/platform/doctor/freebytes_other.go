// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package doctor

// getFreeBytes returns 0 on non-Linux platforms.
// Free-space reporting is not implemented outside Linux.
func getFreeBytes(_ string) int64 {
	return 0
}
