// Package notifier provides Notifier implementations for the veska module.
package notifier

import (
	"context"
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// StderrNotifier is a Notifier that writes notifications to os.Stderr in the
// format "[<level>] <message>\n". It is the default implementation and
// requires no external dependencies.
// StderrNotifier is safe for concurrent use; individual Fprintf calls to
// os.Stderr are atomic on all supported platforms for small writes.
type StderrNotifier struct{}

// Compile-time interface satisfaction check.
var _ ports.Notifier = (*StderrNotifier)(nil)

// NewStderrNotifier constructs a StderrNotifier.
func NewStderrNotifier() *StderrNotifier {
	return &StderrNotifier{}
}

// Notify writes "[<n.Level>] <n.Message>\n" to os.Stderr.
// It respects ctx cancellation — if the context is already done when Notify is
// called, it returns the context error without writing.
func (s *StderrNotifier) Notify(ctx context.Context, n ports.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(os.Stderr, "[%s] %s\n", n.Level, n.Message)
	return err
}
