// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package repo

// CurrentRSS returns 0 on non-Linux platforms where resident set size is not measured.
func CurrentRSS() (int64, error) {
	return 0, nil
}
