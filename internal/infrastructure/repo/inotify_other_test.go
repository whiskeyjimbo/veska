// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package repo

import "testing"

func TestCheckInotifyBudgetDisabled(t *testing.T) {
	budget, err := CheckInotifyBudget(5, 128)
	if err != nil {
		t.Fatalf("unexpected error on non-linux: %v", err)
	}
	if budget.Max != -1 {
		t.Errorf("Max = %d, want -1 (disabled)", budget.Max)
	}
}
