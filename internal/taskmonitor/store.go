package taskmonitor

import "context"

// Store is the read-only query surface for task monitoring.  Every method
// accepts a projectDir that scopes the query to a single project; an empty
// string means "all projects the caller has access to".
//
// Implementations MUST:
//   - Cleanse projectDir (e.g. via filepath.Clean) and reject values
//     that escape the intended root (e.g. "../" traversal).
//   - Not read private session files or internal Reasonix state files.
//   - Sanitise all output (no prompt text, tool arguments, tool results,
//     or reasoning traces).
//   - Return zero-value slices (not nil) for empty result sets.
type Store interface {
	ListTasks(ctx context.Context, projectDir string) ([]TaskSnapshot, error)
	GetTask(ctx context.Context, projectDir string, taskID string) (*TaskSnapshot, error)
	ListEvents(ctx context.Context, projectDir string, taskID string, afterSequence int) ([]TaskEvent, error)
}

// WriteStore extends Store with atomic write operations for control
// commands. It is used by ControlService to persist state changes with
// version-based optimistic locking.
type WriteStore interface {
	Store

	// SaveTask atomically persists snap. It must fail if a concurrent
	// write has already changed the stored version.
	SaveTask(ctx context.Context, projectDir string, snap TaskSnapshot) error

	// SaveEvent appends an audit event for taskID.
	SaveEvent(ctx context.Context, projectDir string, ev TaskEvent) error
}
