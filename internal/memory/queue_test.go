package memory

import (
	"context"
	"testing"
)

type testQueue struct{}

func (testQueue) QueueMemory(string) {}

type preservedQueueContextKey struct{}

func TestWithoutQueueShadowsOnlyQueue(t *testing.T) {
	parent := context.WithValue(WithQueue(context.Background(), testQueue{}), preservedQueueContextKey{}, "preserved")
	child := WithoutQueue(parent)
	if _, ok := QueueFromContext(child); ok {
		t.Fatal("child context inherited the parent memory queue")
	}
	if got := child.Value(preservedQueueContextKey{}); got != "preserved" {
		t.Fatalf("unrelated context value = %v, want preserved", got)
	}

	owned := WithQueue(child, testQueue{})
	if _, ok := QueueFromContext(owned); !ok {
		t.Fatal("child-owned memory queue did not override the shadow value")
	}
}
