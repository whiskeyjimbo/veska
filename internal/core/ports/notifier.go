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

// Notifier is the port for dispatching runtime notifications to the user or an
// external system. Implementations are provided by infrastructure adapters
// (e.g. stderr, desktop notifications, Slack webhooks).
type Notifier interface {
	// Notify dispatches n to the underlying sink. Implementations must be safe
	// for concurrent use and must respect ctx cancellation.
	Notify(ctx context.Context, n Notification) error
}
