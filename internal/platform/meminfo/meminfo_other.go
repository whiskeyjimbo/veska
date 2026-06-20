// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package meminfo

import "errors"

// ErrUnsupported signals that available-RAM reporting is not implemented on
// this platform. Callers must treat it as "unknown" and degrade to a no-op
// (never warn, never pause) rather than assuming any particular memory level.
var ErrUnsupported = errors.New("meminfo: available memory reporting unsupported on this platform")

// Available is unimplemented outside Linux. It returns ErrUnsupported so the
// daemon's memory-pressure checks degrade safely to no-ops, mirroring the
// doctor freebytes_other.go split.
func Available() (uint64, error) {
	return 0, ErrUnsupported
}
