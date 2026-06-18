// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package application

import (
	"errors"
	"fmt"
)

// ErrMissingDependency is returned by constructors when a required dependency is nil.
var ErrMissingDependency = errors.New("application: missing required dependency")

// ErrBusy is returned when a database write operation cannot proceed because the writer pool is full.
type ErrBusy struct {
	Cause     string // "seal_in_flight" | "pool_wait"
	InUse     int    // db.Stats.InUse at time of error
	WaitCount int64  // db.Stats.WaitCount
}

func (e ErrBusy) Error() string {
	return fmt.Sprintf("veska: writer busy (cause=%s, in_use=%d, wait_count=%d)", e.Cause, e.InUse, e.WaitCount)
}

var ErrDaemonStarting = errors.New("veska: daemon starting (startup resync in progress)")
