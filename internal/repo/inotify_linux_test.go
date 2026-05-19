//go:build linux

package repo

import (
	"strings"
	"testing"
)

func TestCheckInotifyBudgetSufficient(t *testing.T) {
	orig := readMaxWatches
	defer func() { readMaxWatches = orig }()
	readMaxWatches = func() (int, error) { return 8192, nil }

	budget, err := CheckInotifyBudget(5, 128)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if budget.Max != 8192 {
		t.Errorf("Max = %d, want 8192", budget.Max)
	}
	if budget.InUse != 5*128 {
		t.Errorf("InUse = %d, want %d", budget.InUse, 5*128)
	}
	if budget.Available <= 0 {
		t.Errorf("Available = %d, want > 0", budget.Available)
	}
}

func TestCheckInotifyBudgetShort(t *testing.T) {
	orig := readMaxWatches
	defer func() { readMaxWatches = orig }()
	readMaxWatches = func() (int, error) { return 512, nil }

	// 5 * 128 = 640 > 512, so budget is short
	_, err := CheckInotifyBudget(5, 128)
	if err == nil {
		t.Fatal("expected error for short budget, got nil")
		return
	}
	if !strings.Contains(err.Error(), "sysctl") {
		t.Errorf("error %q does not contain 'sysctl'", err.Error())
	}
}

func TestInotifyFixCommand(t *testing.T) {
	cmd := InotifyFixCommand()
	if !strings.HasPrefix(cmd, "sudo sysctl") {
		t.Errorf("InotifyFixCommand() = %q, want prefix 'sudo sysctl'", cmd)
	}
}
