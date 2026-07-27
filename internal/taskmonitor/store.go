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
	// ListTasks returns every task snapshot visible under projectDir,
	// ordered by UpdatedAt descending (most-recently-active first).
	ListTasks(ctx context.Context, projectDir string) ([]TaskSnapshot, error)

	// GetTask returns the latest snapshot for taskID under projectDir.
	// Returns nil, nil when the task is not found.
	GetTask(ctx context.Context, projectDir string, taskID string) (*TaskSnapshot, error)

	// ListEvents returns events for taskID under projectDir whose
	// Sequence > afterSequence, ordered by Sequence ascending.
	// Pass 0 for afterSequence to start from the beginning.
	// When the task cannot be found, it returns an empty slice with
	// no error.
	ListEvents(ctx context.Context, projectDir string, taskID string, afterSequence int) ([]TaskEvent, error)
}
