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
// Implementations are provided by infrastructure adapters (e.g. fsnotify).
type Watcher interface {
	// Watch registers the directory tree rooted at dir for change events and
	// returns a channel on which FileEvents are delivered. The channel is closed
	// when ctx is cancelled or Close is called. Calling Watch again on an already
	// watched directory is a no-op and returns the same channel.
	Watch(ctx context.Context, dir string) (<-chan FileEvent, error)

	// Close stops all watches and releases underlying OS resources.
	// The event channel returned by Watch is closed before Close returns.
	Close() error
}
