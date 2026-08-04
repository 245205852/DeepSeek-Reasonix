// Package target implements TargetManager: Local + at most one Remote adapter
// with a single active projection for the main workbench.
//
// Logical Remote slots outlive physical SSH/rpcwire generations so unexpected
// disconnects can reconnect and restore the exact Host session without falling
// back to Local or inventing a different Session.
package target

import (
	"fmt"
	"sync"
	"sync/atomic"

	"reasonix/internal/remote/protocol"
)

// Kind classifies the active workbench projection.
type Kind string

const (
	KindLocal  Kind = "local"
	KindRemote Kind = "ssh"
)

// ConnState is the lifecycle of the logical Remote slot (not the Local
// projection). connecting/connected/reconnecting/reconnect_failed keep kind=ssh
// while the projection remains Remote.
type ConnState string

const (
	StateConnecting      ConnState = "connecting"
	StateConnected       ConnState = "connected"
	StateReconnecting    ConnState = "reconnecting"
	StateReconnectFailed ConnState = "reconnect_failed"
	StateDisconnected    ConnState = "disconnected"
)

// Identity is a stable key for last-click-wins fencing.
type Identity struct {
	Kind      Kind
	HostID    string // empty for local
	Workspace string // empty for local
}

func (i Identity) String() string {
	if i.Kind != KindRemote {
		return "local"
	}
	return string(i.Kind) + ":" + i.HostID + ":" + i.Workspace
}

// RemoteHint is the last connected remote shown after Desktop restart (no auto-connect).
type RemoteHint struct {
	HostID    string
	Workspace string
	Label     string
}

// Manager maintains Local always-on, optional Remote, and one active projection.
type Manager struct {
	mu sync.Mutex

	active      Identity
	identityGen atomic.Uint64
	attachGen   atomic.Uint64
	logicalGen  atomic.Uint64
	requestSeq  atomic.Uint64
	remote      *RemoteState
	connecting  *RemoteState
	lastRemote  RemoteHint
	// busyRemote is true when the remote adapter is running a turn/mutation.
	busyRemote bool
}

// RemoteState is the logical Remote slot. Physical transport liveness is
// Generation + ConnState; snapshot caches live in the Desktop workbench kernel.
type RemoteState struct {
	Identity Identity
	// Connected is true only while ConnState is connected and a physical client
	// is live for Generation.
	Connected bool
	// Generation is the current physical attach generation; late callbacks for
	// older generations are dropped.
	Generation uint64
	// LogicalGen identifies the logical slot across physical reconnects.
	LogicalGen uint64
	// ConnState is the user-visible lifecycle for the Remote slot.
	ConnState ConnState
	// Fingerprint is the authenticated host key that must match on reconnect.
	Fingerprint string
	// Target and RuntimeEpoch are the exact session to restore.
	Target       protocol.RuntimeTarget
	RuntimeEpoch protocol.RuntimeEpoch
	// Attempt is the current automatic reconnect attempt (1-based while active).
	Attempt int
	// Retryable is true when the user may call WorkbenchConnectRemote to retry.
	Retryable bool
	// Error is a non-secret failure summary for the status bar.
	Error string
}

// New returns a Manager that always starts on Local.
func New() *Manager {
	m := &Manager{active: Identity{Kind: KindLocal}}
	m.identityGen.Store(1)
	return m
}

// Active returns the current projection identity and fencing tokens.
func (m *Manager) Active() (Identity, uint64, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, m.identityGen.Load(), m.requestSeq.Load()
}

// Connecting reports whether a candidate Remote adapter is being prepared.
// The committed active identity remains authoritative until activation, but
// callers can use this bit to fence mutations during the transition.
func (m *Manager) Connecting() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connecting != nil
}

// LastRemoteHint is the reconnect entry shown after restart (never auto-SSH).
func (m *Manager) LastRemoteHint() RemoteHint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRemote
}

// RememberRemote updates the post-restart reconnect hint only.
func (m *Manager) RememberRemote(hint RemoteHint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastRemote = hint
}

// SwitchLocal projects the permanent local adapter. Returns new fencing tokens.
// The Remote logical slot may remain for background reconnect; it is not
// cleared by projection switch alone.
func (m *Manager) SwitchLocal() (Identity, uint64, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = Identity{Kind: KindLocal}
	gen := m.identityGen.Add(1)
	seq := m.requestSeq.Add(1)
	return m.active, gen, seq
}

// BeginRemoteConnect records a candidate connection without replacing the
// committed Remote adapter. Returns error if another remote is busy or a
// candidate is already being connected.
//
// When the committed slot is reconnect_failed for the same host/workspace,
// this is a manual retry of the exact saved target (caller still supplies
// recovery details from the workbench kernel cache).
func (m *Manager) BeginRemoteConnect(hostID, workspace string) (Identity, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connecting != nil {
		return Identity{}, 0, fmt.Errorf("remote connection already in progress")
	}
	if m.remote != nil && m.busyRemote && m.remote.Connected {
		return Identity{}, 0, fmt.Errorf("remote target is busy; cancel the turn before switching hosts")
	}
	// Replacing a different host while reconnecting is allowed only when not busy.
	if m.remote != nil && m.busyRemote && m.remote.ConnState == StateReconnecting {
		return Identity{}, 0, fmt.Errorf("remote target is busy; cancel the turn before switching hosts")
	}
	id := Identity{Kind: KindRemote, HostID: hostID, Workspace: workspace}
	gen := m.attachGen.Add(1)
	var logical uint64
	if m.remote != nil && m.remote.Identity.HostID == hostID && m.remote.Identity.Workspace == workspace {
		logical = m.remote.LogicalGen
	} else {
		logical = m.logicalGen.Add(1)
	}
	m.connecting = &RemoteState{
		Identity: id, Connected: false, Generation: gen, LogicalGen: logical,
		ConnState: StateConnecting,
	}
	// Preserve exact session from the failed/reconnecting slot for the same identity.
	if m.remote != nil && m.remote.Identity == id {
		m.connecting.Target = m.remote.Target
		m.connecting.RuntimeEpoch = m.remote.RuntimeEpoch
		m.connecting.Fingerprint = m.remote.Fingerprint
	}
	return id, gen, nil
}

// MarkRemoteConnected marks the remote adapter online for the given generation.
func (m *Manager) MarkRemoteConnected(gen uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connecting == nil || m.connecting.Generation != gen {
		return fmt.Errorf("stale remote generation")
	}
	m.connecting.Connected = true
	m.connecting.ConnState = StateConnected
	m.connecting.Attempt = 0
	m.connecting.Retryable = false
	m.connecting.Error = ""
	return nil
}

// MarkRemoteDisconnected fences a dead transport for the physical generation.
// Unexpected loss keeps the Remote projection and enters reconnecting so the
// UI stays on the last snapshot. Late disconnect callbacks from an older
// connection are ignored.
//
// Returns (active, identityGen, requestSeq, changed).
func (m *Manager) MarkRemoteDisconnected(gen uint64) (Identity, uint64, uint64, bool) {
	return m.markTransportLost(gen, true)
}

// MarkRemoteTransportLost is the explicit form of unexpected disconnect handling.
// When keepProjection is true the active kind stays ssh and ConnState becomes
// reconnecting; when false the projection returns to Local (legacy tests).
func (m *Manager) MarkRemoteTransportLost(gen uint64, keepProjection bool) (Identity, uint64, uint64, bool) {
	return m.markTransportLost(gen, keepProjection)
}

func (m *Manager) markTransportLost(gen uint64, keepProjection bool) (Identity, uint64, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil || m.remote.Generation != gen {
		return m.active, m.identityGen.Load(), m.requestSeq.Load(), false
	}
	// Already not connected for this generation.
	if !m.remote.Connected && m.remote.ConnState != StateConnected {
		// Still accept a transition from connected→lost only once.
		if m.remote.ConnState == StateReconnecting || m.remote.ConnState == StateReconnectFailed {
			return m.active, m.identityGen.Load(), m.requestSeq.Load(), false
		}
	}
	wasConnected := m.remote.Connected || m.remote.ConnState == StateConnected
	if !wasConnected && m.remote.ConnState != StateConnecting {
		return m.active, m.identityGen.Load(), m.requestSeq.Load(), false
	}
	m.remote.Connected = false
	m.busyRemote = false
	// Physical client is gone; generation is retained only for late-callback fence
	// until a new physical generation is committed.
	if keepProjection {
		m.remote.ConnState = StateReconnecting
		m.remote.Attempt = 1
		m.remote.Retryable = true
		m.remote.Error = ""
		if m.active.Kind != KindRemote {
			// Background Remote lifecycle must not invalidate an in-flight Local
			// target token; no Remote mutation can be routed while Local is active.
			return m.active, m.identityGen.Load(), m.requestSeq.Load(), true
		}
		// Advance fencing so in-flight mutations on the visible dead transport fail closed.
		return m.active, m.identityGen.Add(1), m.requestSeq.Add(1), true
	}
	m.remote.ConnState = StateDisconnected
	if m.active.Kind == KindRemote {
		m.active = Identity{Kind: KindLocal}
		return m.active, m.identityGen.Add(1), m.requestSeq.Add(1), true
	}
	return m.active, m.identityGen.Load(), m.requestSeq.Load(), true
}

// BeginAutoReconnect records a supervisor attempt on the logical slot. physicalGen
// must be the new attach generation reserved for the candidate transport.
func (m *Manager) BeginAutoReconnect(logicalGen, physicalGen uint64, attempt int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil || m.remote.LogicalGen != logicalGen {
		return fmt.Errorf("stale logical remote slot")
	}
	if m.remote.ConnState != StateReconnecting && m.remote.ConnState != StateReconnectFailed {
		return fmt.Errorf("remote is not awaiting reconnect")
	}
	if m.connecting != nil {
		return fmt.Errorf("remote connection already in progress")
	}
	slot := *m.remote
	slot.Generation = physicalGen
	slot.Connected = false
	slot.ConnState = StateReconnecting
	slot.Attempt = attempt
	slot.Retryable = true
	slot.Error = ""
	m.connecting = &slot
	return nil
}

// MarkReconnectFailed ends automatic recovery for the logical slot while
// preserving the last snapshot identity. Projection stays Remote when active.
func (m *Manager) MarkReconnectFailed(logicalGen uint64, errText string) (Identity, uint64, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil || m.remote.LogicalGen != logicalGen {
		return m.active, m.identityGen.Load(), m.requestSeq.Load(), false
	}
	m.remote.Connected = false
	m.remote.ConnState = StateReconnectFailed
	m.remote.Retryable = true
	m.remote.Error = errText
	m.busyRemote = false
	if m.connecting != nil && m.connecting.LogicalGen == logicalGen {
		m.connecting = nil
	}
	if m.active.Kind != KindRemote {
		return m.active, m.identityGen.Load(), m.requestSeq.Load(), true
	}
	return m.active, m.identityGen.Add(1), m.requestSeq.Add(1), true
}

// UpdateReconnectAttempt refreshes the attempt counter without changing generation.
func (m *Manager) UpdateReconnectAttempt(logicalGen uint64, attempt int, errText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil || m.remote.LogicalGen != logicalGen {
		return
	}
	if m.remote.ConnState != StateReconnecting {
		return
	}
	m.remote.Attempt = attempt
	m.remote.Error = errText
}

// SetRemoteSession records the exact Host session after a successful subscribe.
func (m *Manager) SetRemoteSession(gen uint64, target protocol.RuntimeTarget, epoch protocol.RuntimeEpoch, fingerprint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	apply := func(rs *RemoteState) {
		if rs == nil || rs.Generation != gen {
			return
		}
		rs.Target = target
		rs.RuntimeEpoch = epoch
		if fingerprint != "" {
			rs.Fingerprint = fingerprint
		}
	}
	apply(m.connecting)
	apply(m.remote)
}

// Remote returns a copy of the current remote lifecycle, including its attach
// generation. Projection identity generations deliberately advance again on
// ActivateRemote, so callers must compare a remote client with this token—not
// with Active's projection token.
func (m *Manager) Remote() *RemoteState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil {
		return nil
	}
	cp := *m.remote
	return &cp
}

// AbortRemoteConnect clears only the generation that failed. It is a no-op for
// a newer replacement, preventing a late failure from tearing down the winner.
func (m *Manager) AbortRemoteConnect(gen uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connecting == nil || m.connecting.Generation != gen {
		return false
	}
	m.connecting = nil
	return true
}

// ActivateRemote projects the connected remote. Drops stale if gen mismatches.
func (m *Manager) ActivateRemote(gen uint64) (Identity, uint64, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connecting != nil && m.connecting.Generation == gen && m.connecting.Connected {
		m.remote = m.connecting
		m.connecting = nil
		m.remote.ConnState = StateConnected
		m.remote.Attempt = 0
		m.remote.Retryable = false
		m.remote.Error = ""
		m.lastRemote = RemoteHint{HostID: m.remote.Identity.HostID, Workspace: m.remote.Identity.Workspace}
	} else if m.remote == nil || m.remote.Generation != gen || !m.remote.Connected {
		return Identity{}, 0, 0, fmt.Errorf("remote not connected")
	}
	m.active = m.remote.Identity
	idGen := m.identityGen.Add(1)
	seq := m.requestSeq.Add(1)
	return m.active, idGen, seq, nil
}

// CommitBackgroundRemote installs a successful reconnect without changing the
// active projection when the user has already switched to Local.
func (m *Manager) CommitBackgroundRemote(gen uint64) (Identity, uint64, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connecting == nil || m.connecting.Generation != gen || !m.connecting.Connected {
		return Identity{}, 0, 0, fmt.Errorf("remote not connected")
	}
	m.remote = m.connecting
	m.connecting = nil
	m.remote.ConnState = StateConnected
	m.remote.Attempt = 0
	m.remote.Retryable = false
	m.remote.Error = ""
	m.lastRemote = RemoteHint{HostID: m.remote.Identity.HostID, Workspace: m.remote.Identity.Workspace}
	// Do not change or fence the Local projection. The target event refreshes the
	// Remote badge without invalidating in-flight Local target tokens.
	return m.active, m.identityGen.Load(), m.requestSeq.Load(), nil
}

// SetRemoteBusy records whether the remote adapter is mid-turn (blocks host swap).
func (m *Manager) SetRemoteBusy(busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busyRemote = busy
}

// DetachRemote clears the remote adapter when idle. Busy remote refuses detach.
// Unreachable but idle remotes may be cleared locally; Host grace-period recovery
// continues independently.
func (m *Manager) DetachRemote() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busyRemote {
		return fmt.Errorf("cannot disconnect while a remote turn is running")
	}
	if m.active.Kind == KindRemote {
		m.active = Identity{Kind: KindLocal}
		m.identityGen.Add(1)
		m.requestSeq.Add(1)
	}
	m.remote = nil
	m.connecting = nil
	return nil
}

// IsStale reports whether a completion for (identityGen, requestSeq) is obsolete.
func (m *Manager) IsStale(identityGen, requestSeq uint64) bool {
	return identityGen != m.identityGen.Load() || requestSeq != m.requestSeq.Load()
}

// RemoteBackground returns the non-active remote if it is still present.
func (m *Manager) RemoteBackground() *RemoteState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remote == nil {
		return nil
	}
	if m.active.Kind == KindRemote && m.active.String() == m.remote.Identity.String() {
		return nil
	}
	cp := *m.remote
	return &cp
}

// NextAttachGeneration reserves a physical generation without starting a connect.
func (m *Manager) NextAttachGeneration() uint64 {
	return m.attachGen.Add(1)
}

// ProjectedRemote reports whether the main window projects a Remote identity
// (including reconnecting / failed read-only states).
func (m *Manager) ProjectedRemote() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active.Kind == KindRemote
}

// ConnectedRemote reports whether a live physical remote client may be used.
func (m *Manager) ConnectedRemote() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remote != nil && m.remote.Connected && m.remote.ConnState == StateConnected
}
