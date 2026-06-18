// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build hnsw_native

package vector_test

import "time"

// timeNow returns the current monotonic clock reading in nanoseconds.
func timeNow() int64 {
	return time.Now().UnixNano()
}
