package application_test

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/engram/solov2/internal/application"
)

func TestErrBusy_Error_Format(t *testing.T) {
	t.Parallel()

	e := &application.ErrBusy{
		Cause:     "seal_in_flight",
		InUse:     1,
		WaitCount: 3,
	}
	got := e.Error()
	want := "engram: writer busy (cause=seal_in_flight, in_use=1, wait_count=3)"
	if got != want {
		t.Errorf("Error() = %q; want %q", got, want)
	}
}

func TestErrBusy_CausePoolWait(t *testing.T) {
	t.Parallel()

	e := &application.ErrBusy{
		Cause:     "pool_wait",
		InUse:     1,
		WaitCount: 10,
	}
	got := e.Error()
	if !strings.Contains(got, "cause=pool_wait") {
		t.Errorf("Error() = %q; want cause=pool_wait in output", got)
	}
}

func TestErrBusy_CauseSealInFlight(t *testing.T) {
	t.Parallel()

	e := &application.ErrBusy{
		Cause:     "seal_in_flight",
		InUse:     0,
		WaitCount: 0,
	}
	got := e.Error()
	if !strings.Contains(got, "cause=seal_in_flight") {
		t.Errorf("Error() = %q; want cause=seal_in_flight in output", got)
	}
}

// Compile-time check: *ErrBusy implements error.
var _ error = (*application.ErrBusy)(nil)

func TestErrBusy_ErrorStringNotEmpty(t *testing.T) {
	t.Parallel()

	e := &application.ErrBusy{Cause: "pool_wait", InUse: 0, WaitCount: 0}
	if e.Error() == "" {
		t.Error("ErrBusy.Error() returned empty string")
	}
}
