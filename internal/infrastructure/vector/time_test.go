//go:build hnsw_native

package vector_test

import "time"

// timeNow returns the current monotonic clock reading in nanoseconds.
func timeNow() int64 {
	return time.Now().UnixNano()
}
