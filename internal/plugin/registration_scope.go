package plugin

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// HostClientRef identifies one live Client instance on a Host. Desktop
// generation-rollback uses RemoveIfInstance so a lost build cannot tear down
// sibling or newer-generation connections that only share a server name.
type HostClientRef struct {
	Name string
	ID   uint64
}

// ErrRegistrationScopeAborted is returned when a connection completes after
// its owning build scope was aborted (generation loss / superseded build).
var ErrRegistrationScopeAborted = errors.New("plugin: registration scope aborted")

type registrationScopeKey struct{}

// RegistrationScope is a per-build ownership token for Host client
// registrations. Only connections that carry this scope via context are
// attributed to the build; sibling hot-adds omit it. Scopes do not serialize
// Host mutations. Abort rejects late LazyToolset registrations.
type RegistrationScope struct {
	host *Host
	id   uint64

	mu      sync.Mutex
	refs    []HostClientRef
	aborted bool
}

// BeginRegistrationScope creates an independent ownership token for one
// controller build. Callers must propagate it with ContextWithRegistrationScope
// on both synchronous and asynchronous MCP connection paths.
func (h *Host) BeginRegistrationScope() *RegistrationScope {
	if h == nil {
		return &RegistrationScope{}
	}
	return &RegistrationScope{
		host: h,
		id:   h.nextScopeID.Add(1),
	}
}

// ContextWithRegistrationScope attaches scope to ctx for EnsureConnected /
// LazyToolset / ReplaceServerBackend ownership attribution.
func ContextWithRegistrationScope(ctx context.Context, scope *RegistrationScope) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, registrationScopeKey{}, scope)
}

// RegistrationScopeFromContext returns the build scope on ctx, if any.
func RegistrationScopeFromContext(ctx context.Context) *RegistrationScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(registrationScopeKey{}).(*RegistrationScope)
	return scope
}

// ID returns the Host-local scope identifier (0 when Host was nil).
func (s *RegistrationScope) ID() uint64 {
	if s == nil {
		return 0
	}
	return s.id
}

// Aborted reports whether AbortAndRollback has been called.
func (s *RegistrationScope) Aborted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// Snapshot returns the client instances attributed to this scope.
func (s *RegistrationScope) Snapshot() []HostClientRef {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HostClientRef(nil), s.refs...)
}

// recordLocked appends a ref when the scope is still live. Returns false when
// aborted so the Host can reject and close the client.
func (s *RegistrationScope) record(ref HostClientRef) bool {
	if s == nil {
		return true // no ownership tracking
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return false
	}
	s.refs = append(s.refs, ref)
	return true
}

// AbortAndRollback marks the scope aborted (rejecting late registrations) and
// removes every instance previously recorded under this scope.
func (s *RegistrationScope) AbortAndRollback() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.aborted = true
	refs := append([]HostClientRef(nil), s.refs...)
	s.mu.Unlock()
	if s.host != nil {
		s.host.RollbackRegistration(refs)
	}
}

// noteClientLocked assigns an instance ID and records ownership on scope.
// Caller holds h.mu. Aborted scopes return ErrRegistrationScopeAborted and do
// not leave c in h.clients (caller closes c).
func (h *Host) noteClientLocked(c *Client, scope *RegistrationScope) error {
	if c == nil {
		return nil
	}
	if c.instanceID == 0 {
		c.instanceID = h.nextInstanceID.Add(1)
	}
	// Append first, then record. Aborted scopes unpublish immediately.
	h.clients = append(h.clients, c)
	if scope != nil {
		if !scope.record(HostClientRef{Name: c.name, ID: c.instanceID}) {
			h.clients = h.clients[:len(h.clients)-1]
			return ErrRegistrationScopeAborted
		}
	}
	return nil
}

// noteClientFromContext is noteClientLocked using the scope on ctx.
func (h *Host) noteClientFromContext(ctx context.Context, c *Client) error {
	return h.noteClientLocked(c, RegistrationScopeFromContext(ctx))
}

// RemoveIfInstance disconnects name only when the live client instance ID still
// matches. Returns whether a matching client was removed.
func (h *Host) RemoveIfInstance(name string, instanceID uint64) bool {
	if h == nil || instanceID == 0 {
		return false
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	h.mu.Lock()
	idx := -1
	var removed *Client
	for i, c := range h.clients {
		if c != nil && c.name == name && c.instanceID == instanceID {
			idx = i
			removed = c
			break
		}
	}
	if idx < 0 || removed == nil {
		h.mu.Unlock()
		return false
	}
	cancels := append([]context.CancelFunc(nil), h.deferredCancels[name]...)
	delete(h.deferredCancels, name)
	if h.deferredGenerations == nil {
		h.deferredGenerations = make(map[string]uint64)
	}
	h.deferredGenerations[name]++
	if h.deferredGenerations[name] == 0 {
		h.deferredGenerations[name] = 1
	}
	h.clients = append(h.clients[:idx], h.clients[idx+1:]...)
	keptP := h.prompts[:0]
	for _, p := range h.prompts {
		if p.Server != name {
			keptP = append(keptP, p)
		}
	}
	h.prompts = keptP
	keptR := h.resources[:0]
	for _, r := range h.resources {
		if r.Server != name {
			keptR = append(keptR, r)
		}
	}
	h.resources = keptR
	h.clearFailure(name)
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	removed.close()
	return true
}

// RollbackRegistration removes every journaled client instance. Safe when a
// ref is already gone (RemoveIfInstance is a no-op).
func (h *Host) RollbackRegistration(refs []HostClientRef) {
	if h == nil {
		return
	}
	for _, ref := range refs {
		h.RemoveIfInstance(ref.Name, ref.ID)
	}
}
