package jobs

import (
	"context"
	"strings"
)

type ctxKey struct{}
type sessionCtxKey struct{}
type jobCtxKey struct{}
type noManager struct{}

// WithManager stamps ctx with the job manager used by background tools.
func WithManager(ctx context.Context, m *Manager) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// WithoutManager shadows an ancestor manager without discarding other values.
func WithoutManager(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, noManager{})
}

func FromContext(ctx context.Context) (*Manager, bool) {
	m, ok := ctx.Value(ctxKey{}).(*Manager)
	return m, ok && m != nil
}

func WithSession(ctx context.Context, parentSession string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, strings.TrimSpace(parentSession))
}

func SessionFromContext(ctx context.Context) string {
	session, _ := ctx.Value(sessionCtxKey{}).(string)
	return strings.TrimSpace(session)
}
