// SPDX-License-Identifier: AGPL-3.0-only

// Package notifier provides Notifier implementations for the veska module.
package notifier

import (
	"context"
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// StderrNotifier writes notifications directly to os.Stderr.
// Writes to os.Stderr are atomic on all supported platforms for small payloads,
// making StderrNotifier safe for concurrent use.
type StderrNotifier struct{}

var _ ports.Notifier = (*StderrNotifier)(nil)

func NewStderrNotifier() *StderrNotifier {
	return &StderrNotifier{}
}

// Notify writes the formatted level and message to os.Stderr.
// It returns a context error without writing if the context is already canceled.
func (s *StderrNotifier) Notify(ctx context.Context, n ports.Notification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(os.Stderr, "[%s] %s\n", n.Level, n.Message)
	return err
}
