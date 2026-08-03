package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/provider"
	"reasonix/internal/remote/broker"
	"reasonix/internal/remote/protocol"
	"reasonix/internal/remote/workbench/client"
	"reasonix/internal/remote/workbench/reconnect"
	"reasonix/internal/remote/workbench/target"
	"reasonix/internal/remote/workbench/transport"
	"reasonix/internal/remote/workbench/trust"
)

// workbenchKernel owns Local + Remote adapters for the main desktop window.
// Logical Remote slot state lives in targets; physical client fields are
// cleared on unexpected disconnect while snapshot caches are retained for
// read-only projection and exact session recovery.
type workbenchKernel struct {
	mu                sync.Mutex
	transitionMu      sync.Mutex
	reconnectMu       sync.Mutex
	targets           *target.Manager
	remote            *client.Client
	remoteGen         uint64
	remoteTabID       string
	remoteFingerprint string
	providerAccess    *workbenchProviderAccess
	snapshot          protocol.SessionSnapshot
	catalog           protocol.WorkspaceCatalogResult
	sessionCatalog    protocol.SessionCatalogResult
	pendingTrust      *ProviderTrustPromptView
	trustAnswer       chan bool
	reconnect         *reconnect.Supervisor
}

// workbenchConnectOptions controls initial connect vs exact auto-reconnect.
type workbenchConnectOptions struct {
	LogicalGen          uint64
	ExactTarget         protocol.RuntimeTarget
	ExactEpoch          protocol.RuntimeEpoch
	RequireExact        bool
	ExpectedFingerprint string
	SkipProviderTrust   bool
	IsAutoReconnect     bool
	Attempt             int
}

type workbenchProviderAccess struct {
	mu      sync.RWMutex
	allowed map[string]struct{}
}

func newWorkbenchProviderAccess(allowed map[string]struct{}) *workbenchProviderAccess {
	access := &workbenchProviderAccess{}
	access.replace(allowed)
	return access
}

func (a *workbenchProviderAccess) snapshot() map[string]struct{} {
	if a == nil {
		return map[string]struct{}{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]struct{}, len(a.allowed))
	for ref := range a.allowed {
		out[ref] = struct{}{}
	}
	return out
}

func (a *workbenchProviderAccess) replace(allowed map[string]struct{}) {
	if a == nil {
		return
	}
	next := make(map[string]struct{}, len(allowed))
	for ref := range allowed {
		next[ref] = struct{}{}
	}
	a.mu.Lock()
	a.allowed = next
	a.mu.Unlock()
}

const workbenchTargetEvent = "remote:workbench-target"

type WorkbenchTargetStateView struct {
	State       string            `json:"state"`
	Kind        target.Kind       `json:"kind"`
	HostID      string            `json:"hostId,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	IdentityGen uint64            `json:"identityGen"`
	RequestSeq  uint64            `json:"requestSeq"`
	Attempt     int               `json:"attempt,omitempty"`
	Retryable   bool              `json:"retryable,omitempty"`
	Error       string            `json:"error,omitempty"`
	Reconnect   target.RemoteHint `json:"reconnect"`
}

// withWorkbenchLocalNavigation serializes a visible Local navigation against
// Remote connect/reactivation. The later caller wins: Remote stays connected in
// the background, while its callbacks stop projecting into the Local surface.
func (a *App) withWorkbenchLocalNavigation(run func() (TabMeta, error)) (meta TabMeta, err error) {
	k := a.workbench()
	k.transitionMu.Lock()
	active, _, _ := k.targets.Active()
	switched := active.Kind == target.KindRemote
	var id target.Identity
	var gen, seq uint64
	if switched {
		id, gen, seq = k.targets.SwitchLocal()
	}
	defer func() {
		if switched {
			tabID := meta.ID
			if tabID == "" {
				tabID = a.workbenchProjectionTabID()
			}
			a.emitWorkbenchTarget("disconnected", id, gen, seq, "")
			a.emitReady(a.ctx, tabID)
			a.emitWorkbenchLocalRuntimeRebuilt(tabID)
		}
		k.transitionMu.Unlock()
	}()
	return run()
}

// ProviderTrustPromptView is the Wails-facing Provider Broker authorization UI.
// Never includes secrets, base URLs, or env names.
type ProviderTrustPromptView struct {
	HostID       string   `json:"hostId"`
	Host         string   `json:"host"`
	KeyType      string   `json:"keyType"`
	Fingerprint  string   `json:"fingerprint"`
	Workspace    string   `json:"workspace"`
	ProviderRefs []string `json:"providerRefs"`
	Warning      string   `json:"warning"`
}

func newWorkbenchKernel() *workbenchKernel {
	return &workbenchKernel{targets: target.New()}
}

func (a *App) workbench() *workbenchKernel {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	if a.workbenchKernel == nil {
		a.workbenchKernel = newWorkbenchKernel()
	}
	return a.workbenchKernel
}

// WorkbenchActiveTarget returns the current projection for the status bar.
// Fields must match remote:workbench-target event payloads (including attempt
// and retryable while reconnecting).
func (a *App) WorkbenchActiveTarget() map[string]any {
	k := a.workbench()
	id, gen, seq := k.targets.Active()
	h := a.workbenchLastRemoteHint()
	out := map[string]any{
		"kind": string(id.Kind), "hostId": id.HostID, "workspace": id.Workspace,
		"identityGen": gen, "requestSeq": seq,
		"reconnect": map[string]string{"hostId": h.HostID, "workspace": h.Workspace, "label": h.Label},
	}
	state := "disconnected"
	if id.Kind == target.KindLocal {
		state = "disconnected"
	}
	if remote := k.targets.Remote(); remote != nil && id.Kind == target.KindRemote {
		state = string(remote.ConnState)
		if state == "" {
			if remote.Connected {
				state = string(target.StateConnected)
			} else {
				state = string(target.StateDisconnected)
			}
		}
		if remote.Attempt > 0 {
			out["attempt"] = remote.Attempt
		}
		if remote.Retryable {
			out["retryable"] = true
		}
		if remote.Error != "" {
			out["error"] = remote.Error
		}
	} else if id.Kind == target.KindRemote {
		state = string(target.StateConnected)
	}
	if k.targets.Connecting() && id.Kind != target.KindRemote {
		state = string(target.StateConnecting)
	}
	out["state"] = state
	return out
}

// WorkbenchLastRemoteHint is the post-restart reconnect entry (no auto-connect).
func (a *App) WorkbenchLastRemoteHint() map[string]string {
	h := a.workbenchLastRemoteHint()
	return map[string]string{"hostId": h.HostID, "workspace": h.Workspace, "label": h.Label}
}

func (a *App) workbenchLastRemoteHint() target.RemoteHint {
	k := a.workbench()
	h := k.targets.LastRemoteHint()
	if h.HostID != "" {
		return h
	}
	remotePrefsMu.Lock()
	prefs := loadRemotePrefs()
	remotePrefsMu.Unlock()
	if prefs.LastHostID == "" {
		return h
	}
	h = target.RemoteHint{HostID: prefs.LastHostID, Workspace: prefs.LastWorkspaceByHost[prefs.LastHostID]}
	if cfg, err := config.Load(); err == nil {
		if entry, ok := cfg.RemoteHost(h.HostID); ok {
			h.Label = entry.Host
		}
	}
	return h
}

// WorkbenchSwitchLocal projects the permanent Local adapter. Background Remote
// recovery may continue, but success must not auto-activate Remote projection.
func (a *App) WorkbenchSwitchLocal() map[string]any {
	k := a.workbench()
	k.transitionMu.Lock()
	defer k.transitionMu.Unlock()
	id, gen, seq := k.targets.SwitchLocal()
	a.emitWorkbenchTarget("disconnected", id, gen, seq, "")
	tabID := a.workbenchProjectionTabID()
	a.emitReady(a.ctx, tabID)
	a.emitWorkbenchLocalRuntimeRebuilt(tabID)
	return map[string]any{"kind": string(id.Kind), "identityGen": gen, "requestSeq": seq, "state": "disconnected"}
}

// WorkbenchConnectRemote opens SSH stdio workbench + local Provider Broker.
// When called against a reconnect_failed slot for the same host/workspace it
// is a manual retry of the saved exact RuntimeTarget.
func (a *App) WorkbenchConnectRemote(hostID, workspace string) error {
	hostID = strings.TrimSpace(hostID)
	workspace = strings.TrimSpace(workspace)
	if hostID == "" || workspace == "" {
		return fmt.Errorf("host and workspace are required")
	}
	a.workbenchStopReconnectSupervisor()
	k := a.workbench()
	k.transitionMu.Lock()
	if remote := k.targets.Remote(); remote != nil && remote.Connected &&
		remote.Identity.HostID == hostID && remote.Identity.Workspace == workspace {
		tabID := a.workbenchProjectionTabID()
		k.mu.Lock()
		cli, previousTabID := k.remote, k.remoteTabID
		if cli != nil && cli.Generation() == remote.Generation {
			k.remoteTabID = tabID
		}
		k.mu.Unlock()
		if cli == nil || cli.Generation() != remote.Generation {
			k.transitionMu.Unlock()
			return fmt.Errorf("Remote adapter is unavailable; reconnect the host")
		}
		// Rebind the projection before activation. Until ActivateRemote succeeds,
		// callbacks still observe Local and cannot leak into the previous tab.
		cli.SetCallbacks(a.workbenchClientCallbacks(remote.Generation, tabID))
		activeID, activeGen, requestSeq, err := k.targets.ActivateRemote(remote.Generation)
		if err != nil {
			k.mu.Lock()
			if k.remote == cli && k.remoteGen == remote.Generation && k.remoteTabID == tabID {
				k.remoteTabID = previousTabID
			}
			k.mu.Unlock()
			cli.SetCallbacks(a.workbenchClientCallbacks(remote.Generation, previousTabID))
			k.transitionMu.Unlock()
			return err
		}
		k.transitionMu.Unlock()
		go a.workbenchRefreshSnapshot(remote.Generation, tabID)
		go a.workbenchRefreshCatalog(remote.Generation)
		a.emitWorkbenchTarget("connected", activeID, activeGen, requestSeq, "")
		a.emitReady(a.ctx, tabID)
		a.emitWorkbenchRuntimeRebuilt(tabID, string(cli.State().RuntimeEpoch))
		return nil
	}
	opts := workbenchConnectOptions{}
	if remote := k.targets.Remote(); remote != nil &&
		remote.Identity.HostID == hostID && remote.Identity.Workspace == workspace &&
		remote.Target.SessionID != "" {
		opts.ExactTarget = remote.Target
		opts.ExactEpoch = remote.RuntimeEpoch
		opts.RequireExact = true
		opts.ExpectedFingerprint = remote.Fingerprint
		opts.LogicalGen = remote.LogicalGen
	}
	k.transitionMu.Unlock()
	return a.workbenchConnectRemoteCore(a.bootContext(), hostID, workspace, opts)
}

func (a *App) workbenchConnectRemoteCore(ctx context.Context, hostID, workspace string, opts workbenchConnectOptions) error {
	k := a.workbench()
	// Reserve the candidate generation under the transition fence, then perform
	// SSH/bootstrap I/O without holding it. This keeps Switch Local and shutdown
	// responsive even when a reconnect attempt is stuck in the network stack.
	k.transitionMu.Lock()
	var gen uint64
	var err error
	if opts.IsAutoReconnect {
		gen = k.targets.NextAttachGeneration()
		if err := k.targets.BeginAutoReconnect(opts.LogicalGen, gen, opts.Attempt); err != nil {
			k.transitionMu.Unlock()
			return err
		}
	} else {
		_, gen, err = k.targets.BeginRemoteConnect(hostID, workspace)
		if err != nil {
			k.transitionMu.Unlock()
			return err
		}
		committedID, committedGen, committedSeq := k.targets.Active()
		a.emitWorkbenchTarget("connecting", committedID, committedGen, committedSeq, "")
	}
	_, intentGen, intentSeq := k.targets.Active()
	k.transitionMu.Unlock()
	committed := false
	failureText := ""
	defer func() {
		if !committed {
			k.transitionMu.Lock()
			if k.targets.AbortRemoteConnect(gen) {
				active, identityGen, requestSeq := k.targets.Active()
				state := "disconnected"
				if active.Kind == target.KindRemote {
					if remote := k.targets.Remote(); remote != nil {
						state = string(remote.ConnState)
						if state == "" {
							state = "connected"
						}
					} else {
						state = "connected"
					}
				}
				a.emitWorkbenchTarget(state, active, identityGen, requestSeq, failureText)
			}
			k.transitionMu.Unlock()
		}
	}()
	fail := func(err error) error {
		failureText = err.Error()
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	entry, ok := cfg.RemoteHost(hostID)
	if !ok {
		return fail(fmt.Errorf("unknown remote host %q", hostID))
	}
	fp, keyType, hostLabel, err := a.workbenchHostIdentity(hostID)
	if err != nil {
		return fail(err)
	}
	if opts.ExpectedFingerprint != "" && fp != opts.ExpectedFingerprint {
		return fail(fmt.Errorf("host key fingerprint changed; refusing automatic reconnect"))
	}
	currentBuild := protocol.CurrentBuildID(version)
	remoteBinary, err := a.workbenchEnsureRemoteCLI(ctx, hostID, entry, currentBuild)
	if err != nil {
		return fail(err)
	}
	refs := localProviderRefs(cfg)
	if len(refs) == 0 {
		return fail(fmt.Errorf("no configured local chat model is available for Remote Workbench"))
	}
	store := trust.DefaultStore()
	missing, err := store.MissingRefs(hostID, fp, refs)
	if err != nil {
		return fail(err)
	}
	if len(missing) > 0 {
		if opts.SkipProviderTrust {
			return fail(fmt.Errorf("provider authorization no longer matches; reconnect explicitly to re-authorize"))
		}
		if err := a.workbenchRequestTrust(hostID, hostLabel, keyType, fp, workspace, missing); err != nil {
			return fail(err)
		}
		if err := store.AuthorizeAll(hostID, keyType, fp, missing); err != nil {
			return fail(err)
		}
	}
	rec, found, err := store.Get(hostID, fp)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(fmt.Errorf("provider authorization was not persisted for host %q", hostID))
	}
	allowed := map[string]struct{}{}
	for _, r := range rec.AllowedProviderRefs {
		allowed[r] = struct{}{}
	}
	if len(allowed) == 0 {
		return fail(fmt.Errorf("no provider model is authorized for host %q", hostID))
	}
	providerAccess := newWorkbenchProviderAccess(allowed)

	// Bind the attach transport to the workspace selected for this connection,
	// not a possibly stale default from the persisted host entry.
	entry.Workspace = workspace
	factory, err := a.workbenchTransportFactory(hostID, entry, remoteBinary)
	if err != nil {
		return fail(err)
	}
	brokerOpts := broker.Options{
		Authorize: func() error {
			return authorizeWorkbenchPeer(factory, fp)
		},
		Catalog: func(ctx context.Context, filter map[string]struct{}) ([]protocol.BrokerProviderDescriptor, error) {
			current, err := config.Load()
			if err != nil {
				return nil, err
			}
			return catalogDescriptors(current, providerAccess.snapshot(), filter)
		},
		Open: func(ctx context.Context, ref, effort string, req provider.Request) (<-chan provider.Chunk, error) {
			if _, ok := providerAccess.snapshot()[ref]; !ok {
				return nil, fmt.Errorf("provider %q not authorized for this host", ref)
			}
			current, err := config.Load()
			if err != nil {
				return nil, err
			}
			requestCtx := provider.WithRequestAttemptCounter(ctx)
			stream, err := openLocalProviderStream(requestCtx, current, ref, effort, req)
			if err != nil {
				recordRemoteProviderUsage(requestCtx, remoteStatsRecorder(), ref, nil)
				return nil, err
			}
			return recordRemoteProviderStream(requestCtx, ref, stream), nil
		},
	}
	buildID := map[string]any{
		"productVersion": currentBuild.ProductVersion, "sourceRevision": currentBuild.SourceRevision,
		"protocolVersion": currentBuild.ProtocolVersion, "schemaHash": currentBuild.SchemaHash,
	}
	tabID := a.workbenchProjectionTabID()
	if tabID == "" {
		k.mu.Lock()
		tabID = k.remoteTabID
		k.mu.Unlock()
	}
	cli, err := client.Connect(ctx, factory, gen, brokerOpts, buildID, workspace, a.workbenchClientCallbacks(gen, tabID))
	if err != nil {
		return fail(err)
	}
	keepClient := false
	defer func() {
		if !keepClient {
			cli.Close()
		}
	}()
	model := strings.TrimSpace(cfg.DefaultModel)
	if entry, ok := cfg.ResolveModel(model); ok {
		model = entry.Name + "/" + entry.Model
	}
	if _, authorized := allowed[model]; !authorized {
		model = ""
	}
	if model == "" && len(refs) > 0 {
		model = refs[0]
	}
	listRaw, err := cli.Request(ctx, string(protocol.MethodSessionList), protocol.SessionListParams{})
	if err != nil {
		return fail(fmt.Errorf("list Remote sessions: %w", err))
	}
	listDecoded, err := protocol.DecodeResult(protocol.MethodSessionList, listRaw)
	if err != nil {
		return fail(fmt.Errorf("decode Remote sessions: %w", err))
	}
	sessions := listDecoded.(protocol.SessionListResult).Items
	if opts.RequireExact {
		foundExact := false
		for _, session := range sessions {
			if session.Target != opts.ExactTarget {
				continue
			}
			epoch := protocol.RuntimeEpoch("")
			if session.Runtime != nil {
				epoch = session.Runtime.RuntimeEpoch
			}
			if epoch == "" {
				return fail(fmt.Errorf("exact Remote session has no runtime epoch"))
			}
			useEpoch := epoch
			if opts.ExactEpoch != "" && epoch != opts.ExactEpoch {
				// Same target still exists but Host replaced the runtime epoch.
				useEpoch = epoch
			} else if opts.ExactEpoch != "" {
				useEpoch = opts.ExactEpoch
				// Prefer Host-advertised epoch when it matches.
				if epoch == opts.ExactEpoch {
					useEpoch = epoch
				}
			}
			if err := cli.SelectSession(session.Target, useEpoch); err != nil {
				return fail(err)
			}
			foundExact = true
			break
		}
		if !foundExact {
			return fail(reconnect.ErrTargetMissing)
		}
	} else {
		resumed := false
		for _, session := range sessions {
			if session.Runtime == nil || session.Runtime.RuntimeEpoch == "" {
				continue
			}
			if err := cli.SelectSession(session.Target, session.Runtime.RuntimeEpoch); err != nil {
				continue
			}
			resumed = true
			break
		}
		if !resumed {
			if _, err := cli.CreateSession(ctx, model, ""); err != nil {
				return fail(fmt.Errorf("create Remote session: %w", err))
			}
		}
	}
	var activeID target.Identity
	var activeGen, requestSeq uint64
	var previous *client.Client
	activateProjection := false
	subscribed, err := cli.SubscribeCommitted(ctx, protocol.HistoryMaxTurns, func(result protocol.SessionSubscribeResult) error {
		// Catalog I/O is still candidate-local and cancellable. Only the atomic
		// generation check + projection swap below needs the transition fence.
		if opts.LogicalGen != 0 {
			if remote := k.targets.Remote(); remote != nil && remote.LogicalGen != opts.LogicalGen && !opts.IsAutoReconnect {
				return fmt.Errorf("stale logical remote slot")
			}
		}
		catalog, err := workbenchLoadCatalog(ctx, cli)
		if err != nil {
			return fmt.Errorf("load Remote model catalog: %w", err)
		}
		sessionCatalog, err := workbenchLoadSessionCatalog(ctx, cli)
		if err != nil {
			return fmt.Errorf("load Remote session catalog: %w", err)
		}
		k.transitionMu.Lock()
		defer k.transitionMu.Unlock()
		activateProjection = !k.targets.IsStale(intentGen, intentSeq)
		if err := k.targets.MarkRemoteConnected(gen); err != nil {
			return err
		}
		if activateProjection {
			activeID, activeGen, requestSeq, err = k.targets.ActivateRemote(gen)
		} else {
			activeID, activeGen, requestSeq, err = k.targets.CommitBackgroundRemote(gen)
		}
		if err != nil {
			return err
		}
		k.targets.SetRemoteSession(gen, result.Snapshot.Target, result.Snapshot.RuntimeEpoch, fp)
		k.mu.Lock()
		previous = k.remote
		k.remote = cli
		k.remoteGen = gen
		if activateProjection || k.remoteTabID == "" {
			k.remoteTabID = tabID
		}
		k.remoteFingerprint = fp
		k.providerAccess = providerAccess
		k.snapshot = result.Snapshot
		k.catalog = catalog
		k.sessionCatalog = sessionCatalog
		k.mu.Unlock()
		k.targets.SetRemoteBusy(result.Snapshot.Runtime.Running || result.Snapshot.Runtime.CurrentOperation != nil)
		if activateProjection && (!opts.IsAutoReconnect || opts.ExactEpoch == "" || result.Snapshot.RuntimeEpoch != opts.ExactEpoch) {
			// Publish Remote authority while SubscribeCommitted still holds the fence.
			a.emitWorkbenchRuntimeRebuilt(tabID, string(result.Snapshot.RuntimeEpoch))
		}
		return nil
	})
	if err != nil {
		return fail(fmt.Errorf("subscribe Remote session: %w", err))
	}
	go a.workbenchMirrorSnapshot(cli, subscribed.Snapshot)
	keepClient = true
	if previous != nil {
		previous.Close()
	}
	k.targets.RememberRemote(target.RemoteHint{HostID: hostID, Workspace: workspace, Label: hostLabel})
	a.saveLastRemoteWorkspace(hostID, workspace)
	committed = true
	if activateProjection {
		a.emitWorkbenchTarget("connected", activeID, activeGen, requestSeq, "")
		a.emitReady(a.ctx, tabID)
	} else {
		// Background recovery only: notify without activating.
		a.emitWorkbenchTargetState("connected", activeID, activeGen, requestSeq, "Remote session restored in the background. Switch back to Remote when ready.", 0, false)
	}
	return nil
}

func (a *App) workbenchEnsureRemoteCLI(ctx context.Context, hostID string, entry config.RemoteHostEntry, expected protocol.BuildID) (string, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return "", err
	}
	manager, ok := rt.(*desktopRemoteManager)
	if !ok {
		return "", fmt.Errorf("remote manager cannot provision the Workbench CLI")
	}
	return manager.EnsureWorkbenchCLI(ctx, hostID, entry, expected)
}

type workbenchPeerIdentitySource interface {
	PeerIdentity() (workbenchPeerIdentity, bool)
}

func authorizeWorkbenchPeer(factory transport.Factory, expectedFingerprint string) error {
	source, ok := factory.(workbenchPeerIdentitySource)
	if !ok {
		return fmt.Errorf("workbench transport cannot report its authenticated peer identity")
	}
	peer, havePeer := source.PeerIdentity()
	if !havePeer || strings.TrimSpace(peer.Fingerprint) == "" || peer.Fingerprint != expectedFingerprint {
		return fmt.Errorf("authenticated workbench peer identity changed during connection")
	}
	return nil
}

// WorkbenchDisconnectRemote detaches when idle and revokes the Broker channel.
// Cancels the auto-reconnect supervisor first. Busy remotes still refuse detach;
// idle but unreachable remotes may clear the local slot.
func (a *App) WorkbenchDisconnectRemote() error {
	a.workbenchStopReconnectSupervisor()
	k := a.workbench()
	k.transitionMu.Lock()
	defer k.transitionMu.Unlock()
	if err := k.targets.DetachRemote(); err != nil {
		return err
	}
	k.mu.Lock()
	if k.remote != nil {
		k.remote.Close()
		k.remote = nil
	}
	k.remoteGen = 0
	k.remoteTabID = ""
	k.remoteFingerprint = ""
	k.providerAccess = nil
	k.snapshot = protocol.SessionSnapshot{}
	k.catalog = protocol.WorkspaceCatalogResult{}
	k.sessionCatalog = protocol.SessionCatalogResult{}
	k.mu.Unlock()
	id, gen, seq := k.targets.Active()
	a.emitWorkbenchTarget("disconnected", id, gen, seq, "")
	tabID := a.workbenchProjectionTabID()
	a.emitReady(a.ctx, tabID)
	a.emitWorkbenchLocalRuntimeRebuilt(tabID)
	return nil
}

// refreshWorkbenchProviderBroker applies the current Desktop provider config to
// an existing Remote capability. Removed providers stop opening immediately;
// newly added refs require an explicit trust decision before the Host sees them.
func (a *App) refreshWorkbenchProviderBroker() error {
	k := a.workbench()
	k.transitionMu.Lock()
	defer k.transitionMu.Unlock()
	remote := k.targets.Remote()
	if remote == nil || !remote.Connected {
		return nil
	}
	k.mu.Lock()
	cli, access, fp := k.remote, k.providerAccess, k.remoteFingerprint
	gen := k.remoteGen
	k.mu.Unlock()
	if cli == nil || access == nil || cli.Generation() != remote.Generation || gen != remote.Generation {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	refs := localProviderRefs(cfg)
	store := trust.DefaultStore()
	missing, err := store.MissingRefs(remote.Identity.HostID, fp, refs)
	if err != nil {
		return err
	}
	var trustErr error
	if len(missing) > 0 {
		liveFingerprint, keyType, hostLabel, identityErr := a.workbenchHostIdentity(remote.Identity.HostID)
		if identityErr != nil {
			trustErr = identityErr
		} else if liveFingerprint != fp {
			trustErr = fmt.Errorf("authenticated workbench peer identity changed during provider refresh")
		} else if requestErr := a.workbenchRequestTrust(remote.Identity.HostID, hostLabel, keyType, fp, remote.Identity.Workspace, missing); requestErr != nil {
			trustErr = requestErr
		} else if authorizeErr := store.AuthorizeAll(remote.Identity.HostID, keyType, fp, missing); authorizeErr != nil {
			trustErr = authorizeErr
		}
	}
	record, _, getErr := store.Get(remote.Identity.HostID, fp)
	if getErr != nil {
		return getErr
	}
	configured := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		configured[ref] = struct{}{}
	}
	allowed := make(map[string]struct{}, len(record.AllowedProviderRefs))
	for _, ref := range record.AllowedProviderRefs {
		if _, ok := configured[ref]; ok {
			allowed[ref] = struct{}{}
		}
	}
	access.replace(allowed)
	if notifyErr := cli.NotifyProviderCatalogChanged(); notifyErr != nil {
		return notifyErr
	}
	go a.workbenchRefreshCatalog(gen)
	return trustErr
}

func (a *App) refreshWorkbenchProviderBrokerAsync() {
	if a == nil || a.ctx == nil {
		return
	}
	go func() {
		if err := a.refreshWorkbenchProviderBroker(); err != nil {
			a.warnForTab(a.workbenchProjectionTabID(), "Remote provider access was not refreshed: "+err.Error())
		}
	}()
}

// WorkbenchRemoteRequest proxies a RuntimeAPI method to the connected remote.
func (a *App) WorkbenchRemoteRequest(method string, paramsJSON string) (string, error) {
	k := a.workbench()
	k.transitionMu.Lock()
	defer k.transitionMu.Unlock()
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindRemote {
		return "", fmt.Errorf("CAPABILITY_UNAVAILABLE: active target is local")
	}
	k.mu.Lock()
	cli := k.remote
	cliGen := k.remoteGen
	k.mu.Unlock()
	remoteState := k.targets.Remote()
	if cli == nil || remoteState == nil || !remoteState.Connected || cliGen != remoteState.Generation {
		return "", remoteReconnectingErr()
	}
	var params any
	if strings.TrimSpace(paramsJSON) != "" {
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return "", err
		}
	} else {
		params = map[string]any{}
	}
	raw, err := cli.Request(a.bootContext(), method, params)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// WorkbenchResolveProviderTrust answers a pending trust prompt.
func (a *App) WorkbenchResolveProviderTrust(accept bool) error {
	k := a.workbench()
	k.mu.Lock()
	ch := k.trustAnswer
	k.trustAnswer = nil
	k.pendingTrust = nil
	k.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("no pending provider trust prompt")
	}
	select {
	case ch <- accept:
	default:
	}
	return nil
}

// WorkbenchPendingProviderTrust returns the current prompt or nil.
func (a *App) WorkbenchPendingProviderTrust() *ProviderTrustPromptView {
	k := a.workbench()
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.pendingTrust
}

func (a *App) workbenchRequestTrust(hostID, hostLabel, keyType, fp, workspace string, refs []string) error {
	k := a.workbench()
	answer := make(chan bool, 1)
	k.mu.Lock()
	if k.trustAnswer != nil {
		k.mu.Unlock()
		return fmt.Errorf("provider trust prompt already pending")
	}
	k.trustAnswer = answer
	k.pendingTrust = &ProviderTrustPromptView{
		HostID: hostID, Host: hostLabel, KeyType: keyType, Fingerprint: fp,
		Workspace: workspace, ProviderRefs: append([]string(nil), refs...),
		Warning: "This remote host will consume your local model API quota through the Provider Broker until disconnect. API keys never leave this machine.",
	}
	prompt := *k.pendingTrust
	k.mu.Unlock()
	if a.ctx != nil {
		a.runtimeEvents.Emit(a.ctx, "remote:provider-trust", prompt)
	}
	select {
	case accept := <-answer:
		if !accept {
			return fmt.Errorf("provider trust declined for host %q", hostID)
		}
		return nil
	case <-a.bootContext().Done():
		return fmt.Errorf("connection closed while waiting for provider trust")
	}
}

func (a *App) workbenchHostIdentity(hostID string) (fingerprint, keyType, hostLabel string, err error) {
	rt, rerr := a.remoteRT()
	if rerr != nil {
		return "", "", "", rerr
	}
	manager, ok := rt.(*desktopRemoteManager)
	if !ok {
		return "", "", "", fmt.Errorf("remote manager cannot provide an authenticated peer identity")
	}
	peer, ok := manager.workbenchPeerIdentity(hostID)
	if !ok {
		return "", "", "", fmt.Errorf("host %q must be connected and host-key verified before opening a workbench", hostID)
	}
	fingerprint, keyType, hostLabel = peer.SHA256, peer.KeyType, peer.Address
	cfg, _ := config.Load()
	if entry, ok := cfg.RemoteHost(hostID); ok {
		if hostLabel == "" {
			hostLabel = entry.Host
			if entry.User != "" {
				hostLabel = entry.User + "@" + entry.Host
			}
		}
	}
	return fingerprint, keyType, hostLabel, nil
}

func (a *App) workbenchTransportFactory(hostID string, entry config.RemoteHostEntry, remoteBinary ...string) (transport.Factory, error) {
	// Windows: system OpenSSH. Other platforms: Go SSH stdio session.
	rt, err := a.remoteRT()
	if err != nil {
		return nil, err
	}
	manager, ok := rt.(*desktopRemoteManager)
	if !ok {
		return nil, fmt.Errorf("remote manager cannot service SSH authentication prompts")
	}
	binary := ""
	if len(remoteBinary) > 0 {
		binary = remoteBinary[0]
	}
	return newWorkbenchSSHFactoryForBinary(entry, binary, manager.workbenchAskPassHandler(hostID, entry))
}

func (a *App) emitWorkbenchTarget(state string, id target.Identity, gen, seq uint64, errText string) {
	attempt, retryable := 0, false
	if remote := a.workbench().targets.Remote(); remote != nil && id.Kind == target.KindRemote {
		attempt = remote.Attempt
		retryable = remote.Retryable || remote.ConnState == target.StateReconnectFailed || remote.ConnState == target.StateReconnecting
	}
	a.emitWorkbenchTargetState(state, id, gen, seq, errText, attempt, retryable)
}

func (a *App) emitWorkbenchTargetState(state string, id target.Identity, gen, seq uint64, errText string, attempt int, retryable bool) {
	if a == nil || a.ctx == nil {
		return
	}
	a.runtimeEvents.Emit(a.ctx, workbenchTargetEvent, WorkbenchTargetStateView{
		State: state, Kind: id.Kind, HostID: id.HostID, Workspace: id.Workspace,
		IdentityGen: gen, RequestSeq: seq, Attempt: attempt, Retryable: retryable,
		Error: errText, Reconnect: a.workbenchLastRemoteHint(),
	})
}

func localProviderRefs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	refs := make([]string, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		pe := &cfg.Providers[i]
		if !modelProviderAccessAllowed(cfg.Desktop.ProviderAccess, pe.Name) || !pe.Configured() {
			continue
		}
		models := pe.ChatModelList()
		if len(models) == 0 {
			if model := pe.DefaultModel(); model != "" {
				models = []string{model}
			}
		}
		for _, model := range models {
			refs = append(refs, pe.Name+"/"+model)
		}
	}
	return refs
}

func catalogDescriptors(cfg *config.Config, allowed, filter map[string]struct{}) ([]protocol.BrokerProviderDescriptor, error) {
	var out []protocol.BrokerProviderDescriptor
	for _, ref := range localProviderRefs(cfg) {
		if _, ok := allowed[ref]; !ok {
			continue
		}
		if len(filter) > 0 {
			if _, ok := filter[ref]; !ok {
				continue
			}
		}
		pe, ok := cfg.ResolveModel(ref)
		if !ok {
			continue
		}
		p, err := boot.NewProviderWithProxy(pe, cfg.NetworkProxySpec())
		if err != nil {
			out = append(out, protocol.BrokerProviderDescriptor{Ref: ref, DisplayName: pe.Name, Model: pe.Model})
			continue
		}
		out = append(out, broker.DescriptorFromProvider(ref, pe.Name, pe.Model, p, pe.SupportedEfforts, config.EffectiveEffort(pe), config.EffectiveVision(pe), pe.ContextWindow, pe.Price))
	}
	return out, nil
}

func openLocalProviderStream(ctx context.Context, cfg *config.Config, ref, effort string, req provider.Request) (<-chan provider.Chunk, error) {
	pe, ok := cfg.ResolveModel(ref)
	if !ok || !pe.Configured() {
		return nil, fmt.Errorf("provider model %q is not configured", ref)
	}
	if strings.TrimSpace(effort) != "" {
		normalized, err := config.NormalizeEffort(pe, effort)
		if err != nil {
			return nil, err
		}
		pe.Effort = normalized
	}
	p, err := boot.NewProviderWithProxy(pe, cfg.NetworkProxySpec())
	if err != nil {
		return nil, err
	}
	return p.Stream(ctx, req)
}
