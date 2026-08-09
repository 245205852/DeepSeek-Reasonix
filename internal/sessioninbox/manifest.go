package sessioninbox

import (
	"encoding/json"
	"time"
)

// manifest is the on-disk revisioned metadata file (no bodies).
type manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Revision      int64             `json:"revision"`
	RunID         string            `json:"runId,omitempty"`
	Paused        bool              `json:"paused"`
	Recovered     bool              `json:"recovered"`
	RecoveredN    int               `json:"recoveredCount,omitempty"`
	Items         []InboxItemMeta   `json:"items"`
	Idempotency   map[string]string `json:"idempotency,omitempty"` // key -> itemID
	UpdatedAt     time.Time         `json:"updatedAt"`
}

func emptyManifest(runID string) *manifest {
	return &manifest{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		Items:         []InboxItemMeta{},
		Idempotency:   map[string]string{},
		UpdatedAt:     time.Now().UTC(),
	}
}

func (m *manifest) clone() *manifest {
	if m == nil {
		return emptyManifest("")
	}
	out := *m
	out.Items = append([]InboxItemMeta(nil), m.Items...)
	if m.Idempotency != nil {
		out.Idempotency = make(map[string]string, len(m.Idempotency))
		for k, v := range m.Idempotency {
			out.Idempotency[k] = v
		}
	} else {
		out.Idempotency = map[string]string{}
	}
	return &out
}

func (m *manifest) totalBytes() int64 {
	var n int64
	for _, it := range m.Items {
		n += it.ByteSize
	}
	return n
}

func (m *manifest) indexOf(id string) int {
	for i, it := range m.Items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

func (m *manifest) item(id string) (InboxItemMeta, bool) {
	i := m.indexOf(id)
	if i < 0 {
		return InboxItemMeta{}, false
	}
	return m.Items[i], true
}

func (m *manifest) setItem(it InboxItemMeta) {
	i := m.indexOf(it.ID)
	if i < 0 {
		m.Items = append(m.Items, it)
		return
	}
	m.Items[i] = it
}

func (m *manifest) removeItem(id string) (InboxItemMeta, bool) {
	i := m.indexOf(id)
	if i < 0 {
		return InboxItemMeta{}, false
	}
	it := m.Items[i]
	m.Items = append(m.Items[:i], m.Items[i+1:]...)
	if it.Idempotency != "" && m.Idempotency != nil {
		if m.Idempotency[it.Idempotency] == id {
			delete(m.Idempotency, it.Idempotency)
		}
	}
	return it, true
}

func decodeManifest(data []byte) (*manifest, error) {
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Items == nil {
		m.Items = []InboxItemMeta{}
	}
	if m.Idempotency == nil {
		m.Idempotency = map[string]string{}
	}
	return &m, nil
}
