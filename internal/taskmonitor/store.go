package taskmonitor

import "context"

// Store is the read-only query surface for task monitoring.
type Store interface {
	ListTasks(ctx context.Context, projectDir string) ([]TaskSnapshot, error)
	GetTask(ctx context.Context, projectDir string, taskID string) (*TaskSnapshot, error)
	ListEvents(ctx context.Context, projectDir string, taskID string, afterSequence int) ([]TaskEvent, error)
}

// IdempotencyRecord captures the binding between an idempotency key and the
// operation it was used for. When a key is reused, all fields must match.
type IdempotencyRecord struct {
	Key     string `json:"key"`
	Op      string `json:"op"`
	TaskID  string `json:"task_id"`
	Version uint64 `json:"version"`
}

// WriteStore extends Store with atomic write operations for control
// commands, persistent idempotency, and event sequencing.
type WriteStore interface {
	Store

	// SaveTask atomically persists snap with version-based CAS.
	SaveTask(ctx context.Context, projectDir string, snap TaskSnapshot) error

	// NextSequence returns the next monotonic sequence number for taskID's
	// event log. Implementations must be thread-safe.
	NextSequence(ctx context.Context, projectDir string, taskID string) (int, error)

	// SaveEvent appends an audit event for taskID.
	SaveEvent(ctx context.Context, projectDir string, ev TaskEvent) error

	// CheckIdempotency returns the recorded key if it exists, or nil.
	// Implementations must persist records across restarts.
	CheckIdempotency(ctx context.Context, projectDir string, key string) (*IdempotencyRecord, error)

	// RecordIdempotency persists an idempotency record. Must be atomic
	// so that a second call with a different record for the same key fails.
	RecordIdempotency(ctx context.Context, projectDir string, r IdempotencyRecord) error
}
