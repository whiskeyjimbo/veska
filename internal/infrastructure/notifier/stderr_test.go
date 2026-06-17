package notifier_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/notifier"
)

var _ ports.Notifier = (*notifier.StderrNotifier)(nil)

func TestStderrNotifier_Notify_DoesNotError(t *testing.T) {
	t.Parallel()
	n := notifier.NewStderrNotifier()

	err := n.Notify(context.Background(), ports.Notification{
		Level:   "INFO",
		Message: "test notification",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStderrNotifier_Notify_CancelledContext(t *testing.T) {
	t.Parallel()
	n := notifier.NewStderrNotifier()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.Notify(ctx, ports.Notification{Level: "WARN", Message: "cancelled"})
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
		return
	}
}
