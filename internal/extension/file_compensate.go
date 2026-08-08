package extension

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FilePriorStore holds prior file contents for compensatable write_file
// receipts so recovery can restore them (never claim success without apply).
type FilePriorStore struct {
	mu   sync.Mutex
	byID map[string]filePrior
}

type filePrior struct {
	Path    string
	Content []byte
	Existed bool
}

// DefaultFilePriorStore belongs to the compatibility runtime owner.
var DefaultFilePriorStore = DefaultRuntimeOwner.FilePriors

// NewFilePriorStore returns an empty store.
func NewFilePriorStore() *FilePriorStore {
	return &FilePriorStore{byID: make(map[string]filePrior)}
}

// Capture records prior content for path under receipt id.
func (s *FilePriorStore) Capture(id, path string, content []byte, existed bool) {
	if s == nil || id == "" || path == "" {
		return
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	s.mu.Lock()
	s.byID[id] = filePrior{Path: path, Content: cp, Existed: existed}
	s.mu.Unlock()
}

// Compensate restores the prior content (or removes a created file). Returns
// error if unknown or IO fails; updates receipt compensation status when store
// is DefaultReceiptStore-linked via caller.
func (s *FilePriorStore) Compensate(id string) error {
	if s == nil {
		return fmt.Errorf("extension: nil file prior store")
	}
	s.mu.Lock()
	prior, ok := s.byID[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("extension: no prior captured for %s", id)
	}
	if !prior.Existed {
		if err := os.Remove(prior.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(prior.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(prior.Path, prior.Content, 0o644)
}

// Forget drops a prior entry after successful compensation.
func (s *FilePriorStore) Forget(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
}

// ApplyFileWriteCompensation restores prior content and marks the receipt applied.
func ApplyFileWriteCompensation(receiptID string) error {
	return RuntimeOwnerOrDefault(nil).ApplyFileWriteCompensation(receiptID)
}
