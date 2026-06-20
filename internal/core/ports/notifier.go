// SPDX-License-Identifier: AGPL-3.0-only

package ports

import "context"

// Notification is a single message dispatched to a Notifier.
type Notification struct {
	// Level is a severity or category label for the notification
	// (e.g. "INFO", "WARN", "ERROR").
	Level string

	// Message is the human-readable notification body.
	Message string
}

// Notifier dispatches runtime notifications to the user or an external system.
type Notifier interface {
	// Notify dispatches n to the underlying sink. Implementations must be safe
	// for concurrent use and must respect context cancellation.
	Notify(ctx context.Context, n Notification) error
}
