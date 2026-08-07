package extension

import (
	"sync"
	"time"
)

// ReceiptStore is a process-local ledger of irreversible / compensatable
// effects used by recovery to decide what is safe to resume. It does not
// claim rollback success for irreversible work.
type ReceiptStore struct {
	mu       sync.Mutex
	byID     map[string]EffectReceipt
	byGen    map[uint64][]string
	sequence uint64
}

// DefaultReceiptStore is the process-wide ledger.
var DefaultReceiptStore = NewReceiptStore()

// NewReceiptStore returns an empty store.
func NewReceiptStore() *ReceiptStore {
	return &ReceiptStore{byID: make(map[string]EffectReceipt), byGen: make(map[uint64][]string)}
}

// Record inserts or updates a receipt. Irreversible receipts never set
// CompensationStatus to a successful rollback.
func (s *ReceiptStore) Record(r EffectReceipt) {
	if s == nil {
		return
	}
	if r.Class == Irreversible {
		// Never claim external work was undone.
		if r.CompensationStatus == "applied" || r.CompensationStatus == "rolled_back" {
			r.CompensationStatus = "not_applicable"
		}
		if r.CompensationStatus == "" {
			r.CompensationStatus = "not_applicable"
		}
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		s.sequence++
		r.ID = "receipt-" + itoaU64(s.sequence)
	}
	s.byID[r.ID] = r
	s.byGen[r.Generation] = append(s.byGen[r.Generation], r.ID)
}

// Get returns a receipt by id.
func (s *ReceiptStore) Get(id string) (EffectReceipt, bool) {
	if s == nil {
		return EffectReceipt{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	return r, ok
}

// ForGeneration returns all receipts for a generation.
func (s *ReceiptStore) ForGeneration(gen uint64) []EffectReceipt {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.byGen[gen]
	out := make([]EffectReceipt, 0, len(ids))
	for _, id := range ids {
		if r, ok := s.byID[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Recoverability classifies whether a generation's external effects allow
// a clean resume. Irreversible completed work without compensation blocks
// claiming a clean rollback but still allows resume with awareness.
type Recoverability struct {
	Clean          bool     `json:"clean"`
	HasIrreversible bool    `json:"hasIrreversible"`
	Blocking       []string `json:"blocking,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// AssessRecoverability reports whether checkpoint resume can claim a clean
// state for generation gen.
func (s *ReceiptStore) AssessRecoverability(gen uint64) Recoverability {
	out := Recoverability{Clean: true}
	for _, r := range s.ForGeneration(gen) {
		switch r.Class {
		case Irreversible:
			out.HasIrreversible = true
			out.Clean = false
			out.Notes = append(out.Notes, "irreversible effect "+r.ID+" cannot be rolled back")
		case Compensatable:
			if r.CompensationStatus == "failed" || r.CompensationStatus == "" {
				out.Clean = false
				out.Blocking = append(out.Blocking, r.ID)
				out.Notes = append(out.Notes, "compensatable effect "+r.ID+" not fully compensated")
			}
		}
	}
	return out
}

// IngestScope copies completed receipts from a LiveScope/EffectScope into the store.
func (s *ReceiptStore) IngestScope(scope EffectScope) {
	if s == nil || scope == nil {
		return
	}
	for _, r := range scope.Receipts() {
		s.Record(r)
	}
}

func itoaU64(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
