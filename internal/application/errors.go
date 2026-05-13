package application

import (
	"errors"
	"fmt"
)

// ErrBusy is returned when a write operation cannot proceed because the writer
// pool is at capacity. Cause is "seal_in_flight" when a promotion barrier is
// active, or "pool_wait" when the pool's wait queue is backed up.
type ErrBusy struct {
	Cause     string // "seal_in_flight" | "pool_wait"
	InUse     int    // db.Stats().InUse at time of error
	WaitCount int64  // db.Stats().WaitCount
}

func (e *ErrBusy) Error() string {
	return fmt.Sprintf("engram: writer busy (cause=%s, in_use=%d, wait_count=%d)", e.Cause, e.InUse, e.WaitCount)
}

// ErrDaemonStarting is returned by write operations when the daemon is still
// running startup resync.
var ErrDaemonStarting = errors.New("engram: daemon starting (startup resync in progress)")
