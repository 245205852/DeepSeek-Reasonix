package jobs

import "context"

type noManager struct{}

// WithoutManager shadows an ancestor manager while preserving the rest of the
// context chain. Agents without Jobs must not accidentally operate a parent's
// background jobs through inherited call context.
func WithoutManager(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, noManager{})
}
