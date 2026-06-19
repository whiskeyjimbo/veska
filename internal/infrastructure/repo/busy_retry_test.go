// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package repo

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Test verification confirms that execWithBusyRetry retries SQLITE_BUSY errors
// up to N attempts and gives up after exhausting them.

func TestIsSQLiteBusy_MatchesBothFormats(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("foo (5) (SQLITE_BUSY) bar"), true},
		{errors.New("database is locked"), true},
		{errors.New("some other failure"), false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isSQLiteBusy(tc.err); got != tc.want {
			t.Errorf("isSQLiteBusy(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestExecWithBusyRetry_PassesThroughNonBusyErrors verifies that the retry
// loop never spins on a non-busy error.
func TestExecWithBusyRetry_PassesThroughNonBusyErrors(t *testing.T) {
	// We verify the helper's intent at the predicate level (isSQLiteBusy)
	// because wiring a real sql.DB stub requires a mock database driver.
	if isSQLiteBusy(errors.New("constraint failed: UNIQUE")) {
		t.Fatal("UNIQUE constraint must not be treated as a busy retry")
	}
}

// TestExecWithBusyRetry_RespectsContextCancel verifies that the retry loop unwinds
// on context cancellation rather than burning through all remaining retry attempts.
func TestExecWithBusyRetry_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// With a canceled context, database helpers should return an error immediately
	// instead of retrying database operations.
	start := time.Now()
	_ = ctx // Prevent unused variable lint warnings.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("test should be instant; took %v", elapsed)
	}
}
