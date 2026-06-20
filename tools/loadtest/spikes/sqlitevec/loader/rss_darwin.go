// SPDX-License-Identifier: AGPL-3.0-only

//go:build darwin

package loader

import "syscall"

// ReadRSSBytes returns peak process RSS in bytes via getrusage.
// On Darwin, ru_maxrss is reported in bytes.
func ReadRSSBytes() int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return usage.Maxrss
}
