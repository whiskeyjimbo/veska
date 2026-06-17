package ports

import "context"

// WatchOp classifies the kind of filesystem change reported by a Watcher.
type WatchOp string

const (
	// WatchOpWrite is emitted when a file is created or its contents change.
	WatchOpWrite WatchOp = "write"

	// WatchOpRemove is emitted when a file is deleted or moved away.
	WatchOpRemove WatchOp = "remove"
)

// FileEvent carries a single filesystem change notification from a Watcher.
type FileEvent struct {
	// Path is the absolute filesystem path of the affected file.
	Path string

	// Op is the kind of change that occurred.
	Op WatchOp
}

// Watcher is the port for filesystem change notification.
type Watcher interface {
	// Watch registers a directory tree for change events. Calling Watch again
	// on an already watched directory is a no-op that returns the same channel.
	Watch(ctx context.Context, dir string) (<-chan FileEvent, error)

	// Close stops all watches and releases underlying OS resources. The event
	// channel returned by Watch is closed before Close returns.
	Close() error
}
