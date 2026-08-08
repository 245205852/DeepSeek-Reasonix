package jobs

import (
	"context"
	"testing"

	"reasonix/internal/event"
)

type preservedContextKey struct{}

func TestWithoutManagerShadowsOnlyManager(t *testing.T) {
	manager := NewManager(event.Discard)
	defer manager.Close()
	parent := context.WithValue(WithManager(context.Background(), manager), preservedContextKey{}, "preserved")
	child := WithoutManager(parent)
	if _, ok := FromContext(child); ok {
		t.Fatal("child context inherited a disabled parent job manager")
	}
	if got := child.Value(preservedContextKey{}); got != "preserved" {
		t.Fatalf("unrelated context value = %v, want preserved", got)
	}
}
