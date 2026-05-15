package ports

import "context"

// TodoEntry is a single row returned by TodoQuerier.FindTodos: the
// branch-stable finding_id, the file anchor, the originating rule
// (always "todo"), the message body and the open/closed state.
type TodoEntry struct {
	FindingID string
	RepoID    string
	Branch    string
	FilePath  string
	Message   string
	State     string
	CreatedAt int64
}

// TodoQuerier is the read-side port for parser-emitted TODO findings.
// Implementations sit on top of the findings table and filter
// rule='todo' with the additional scopes the caller supplies.
//
// The port is intentionally distinct from FindingStorage: TODO retrieval
// has a narrow projection and a single rule, so widening FindingStorage
// to support arbitrary list queries would couple it to display concerns.
type TodoQuerier interface {
	// FindTodos returns every open 'todo' finding for (repoID, branch).
	// If onlyOpen is false closed rows are included too.
	FindTodos(ctx context.Context, repoID, branch string, onlyOpen bool) ([]TodoEntry, error)
}
