package extension

import (
	"context"
	"testing"
)

func TestReceiptStoreIrreversibleNeverRolledBack(t *testing.T) {
	s := NewReceiptStore()
	s.Record(EffectReceipt{ID: "msg-1", Class: Irreversible, Generation: 3, CompensationStatus: "rolled_back"})
	r, ok := s.Get("msg-1")
	if !ok || r.CompensationStatus != "not_applicable" {
		t.Fatalf("receipt = %+v", r)
	}
	rec := s.AssessRecoverability(3)
	if rec.Clean || !rec.HasIrreversible {
		t.Fatalf("recoverability = %+v", rec)
	}
}

func TestReceiptStoreIngestScope(t *testing.T) {
	scope := NewEffectScope(7)
	_ = scope.Track(Effect{
		ID: "file-write", Owner: "test", Class: Compensatable,
		Dispose:    func(context.Context) error { return nil },
		Compensate: func(context.Context) error { return nil },
	})
	_ = scope.Dispose(context.Background())
	s := NewReceiptStore()
	s.IngestScope(scope)
	if len(s.ForGeneration(7)) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(s.ForGeneration(7)))
	}
}

func TestReceiptStoreKeepsSameIDAcrossGenerations(t *testing.T) {
	s := NewReceiptStore()
	s.Record(EffectReceipt{ID: "shared", Owner: "old", Generation: 1, Class: Irreversible})
	s.Record(EffectReceipt{ID: "shared", Owner: "new", Generation: 2, Class: Irreversible})

	oldReceipts := s.ForGeneration(1)
	newReceipts := s.ForGeneration(2)
	if len(oldReceipts) != 1 || oldReceipts[0].Owner != "old" || oldReceipts[0].ID != "shared" {
		t.Fatalf("old receipts = %+v", oldReceipts)
	}
	if len(newReceipts) != 1 || newReceipts[0].Owner != "new" || newReceipts[0].ID == "shared" {
		t.Fatalf("new receipts = %+v", newReceipts)
	}
	if old, ok := s.Get("shared"); !ok || old.Owner != "old" {
		t.Fatalf("original receipt was overwritten: %+v ok=%v", old, ok)
	}
}
