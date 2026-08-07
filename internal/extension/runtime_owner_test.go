package extension

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeOwnerRepeatedFileWritesKeepDistinctPriors(t *testing.T) {
	owner := NewRuntimeOwner()
	owner.Gate.Publish(7)
	path := filepath.Join(t.TempDir(), "same.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := owner.RecordFileWrite(path, true, []byte("old"))
	if err := os.WriteFile(path, []byte("middle"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := owner.RecordFileWrite(path, true, []byte("middle"))
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatal("repeated writes must receive distinct receipt IDs")
	}
	if receipts := owner.Receipts.ForGeneration(7); len(receipts) != 2 {
		t.Fatalf("receipts = %+v", receipts)
	}
	if err := owner.ApplyFileWriteCompensation(second); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "middle" {
		t.Fatalf("second prior = %q", got)
	}
	if err := owner.ApplyFileWriteCompensation(first); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "old" {
		t.Fatalf("first prior = %q", got)
	}
	for _, id := range []string{first, second} {
		r, ok := owner.Receipts.Get(id)
		if !ok || r.CompensationStatus != "applied" {
			t.Fatalf("receipt %s = %+v, ok=%v", id, r, ok)
		}
	}
}
