package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/remote/protocol"
	"reasonix/internal/remote/workbench/client"
	"reasonix/internal/remote/workbench/mirror"
	"reasonix/internal/remote/workbench/reconnect"
	"reasonix/internal/remote/workbench/target"
)

// This file is the native adapter between the frozen Remote protocol and the
// existing main-window AppBindings. Remote is an execution target, not a
// second application: the same React tree hydrates from these projections and
// receives the same agent:event stream it already uses for Local sessions.

func (a *App) workbenchProjectionTabID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if tab := a.tabByIDLocked(""); tab != nil {
		return tab.ID
	}
	return a.activeTabID
}

func (a *App) emitWorkbenchRuntimeRebuilt(tabID, runtimeEpoch string) {
	if strings.TrimSpace(runtimeEpoch) == "" {
		a.emitRuntimeEvent("runtime:rebuilt", tabID)
		return
	}
	a.emitRuntimeEvent("runtime:rebuilt", tabID, runtimeEpoch)
}

func (a *App) emitWorkbenchLocalRuntimeRebuilt(tabID string) {
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	runtimeEpoch := a.sessionRuntimeViewLocked(tab).Epoch
	a.mu.RUnlock()
	if tab == nil || strings.TrimSpace(runtimeEpoch) == "" {
		a.emitWorkbenchRuntimeRebuilt(tabID, runtimeEpoch)
		return
	}
	// Use the tab event lane so the Local authority transition stays ordered
	// with Local agent events. If a concurrent controller rebuild advanced the
	// epoch after our snapshot, publish the newer authority once more.
	a.notifyTabRuntimeRebuiltAtEpoch(tab, runtimeEpoch)
	a.mu.RLock()
	currentEpoch := a.sessionRuntimeViewLocked(tab).Epoch
	a.mu.RUnlock()
	if currentEpoch != "" && currentEpoch != runtimeEpoch {
		a.notifyTabRuntimeRebuiltAtEpoch(tab, currentEpoch)
	}
}

func (a *App) workbenchClientCallbacks(generation uint64, tabID string) client.Callbacks {
	return client.Callbacks{
		OnSessionEvent: func(notification protocol.SessionEvent) {
			k := a.workbench()
			busyChanged := false
			busy := false
			k.mu.Lock()
			if k.remoteGen != generation {
				k.mu.Unlock()
				return
			}
			k.snapshot.BoundarySeq = notification.Seq
			k.snapshot.Runtime.LiveEvents = append(k.snapshot.Runtime.LiveEvents, notification.Event)
			switch notification.Event.Kind {
			case "turn_started":
				busyChanged, busy = true, true
				k.snapshot.Runtime.Running = true
				k.snapshot.Runtime.CurrentTurn = &protocol.TurnState{TurnID: notification.TurnID}
			case "turn_done":
				busyChanged, busy = true, false
				k.snapshot.Runtime.Running = false
				k.snapshot.Runtime.CurrentTurn = nil
				k.snapshot.Runtime.CancelRequested = false
				k.snapshot.PendingPrompt = nil
			case "approval_request":
				if approval := notification.Event.Approval; approval != nil {
					reason := approval.Reason
					k.snapshot.PendingPrompt = &protocol.PendingPrompt{Kind: protocol.PromptApproval, Approval: &protocol.ApprovalPrompt{
						PromptID: protocol.PromptID(approval.ID), Tool: approval.Tool, Subject: approval.Subject,
						Reason: &reason, Fresh: approval.Fresh,
						AllowedDecisions: []protocol.PromptDecision{protocol.DecisionAllowOnce, protocol.DecisionAllowSession, protocol.DecisionAllowPersistent, protocol.DecisionDeny},
					}}
				}
			case "ask_request":
				if ask := notification.Event.Ask; ask != nil {
					k.snapshot.PendingPrompt = &protocol.PendingPrompt{Kind: protocol.PromptAsk, Ask: &protocol.AskPrompt{PromptID: protocol.PromptID(ask.ID)}}
				}
			}
			k.mu.Unlock()
			if notification.Event.Kind == "turn_done" {
				recordRemoteTurnCompletion()
			}
			if busyChanged {
				k.targets.SetRemoteBusy(busy)
			}
			k.transitionMu.Lock()
			active, identityGen, requestSeq := k.targets.Active()
			visible := active.Kind == target.KindRemote && active.HostID != "" && active.Workspace != ""
			if !visible {
				switch notification.Event.Kind {
				case "approval_request", "ask_request", "turn_done":
					a.emitWorkbenchTarget("background_activity", active, identityGen, requestSeq, "Remote session has background activity.")
				}
				k.transitionMu.Unlock()
				return
			}
			k.mu.Lock()
			projectionTabID := k.remoteTabID
			generationCurrent := k.remoteGen == generation
			k.mu.Unlock()
			if !generationCurrent {
				k.transitionMu.Unlock()
				return
			}
			if projectionTabID == "" {
				projectionTabID = tabID
			}
			if a.ctx != nil {
				a.runtimeEvents.Emit(a.ctx, "agent:event", workbenchWireEvent(notification, projectionTabID))
			}
			k.transitionMu.Unlock()
			if notification.Event.Kind == "turn_done" {
				go a.workbenchRefreshSnapshot(generation, projectionTabID)
			}
		},
		OnResyncRequired: func(protocol.SessionResyncRequired) {
			go a.workbenchRefreshSnapshot(generation, tabID)
		},
		OnCatalogChanged: func(protocol.CatalogChanged) {
			go a.workbenchRefreshCatalog(generation)
		},
		OnDisconnected: func() {
			a.workbenchHandleTransportLost(generation, tabID)
		},
	}
}

// workbenchHandleTransportLost clears the physical client, keeps the logical
// Remote slot and last snapshot, and starts the auto-reconnect supervisor.
// Explicit disconnect and App shutdown cancel the supervisor instead.
func (a *App) workbenchHandleTransportLost(generation uint64, tabID string) {
	k := a.workbench()
	id, identityGen, requestSeq, changed := k.targets.MarkRemoteDisconnected(generation)
	if !changed {
		return
	}
	k.mu.Lock()
	if k.remoteGen != generation && k.remoteGen != 0 {
		k.mu.Unlock()
		return
	}
	projectionTabID := k.remoteTabID
	if projectionTabID == "" {
		projectionTabID = tabID
	}
	// Preserve snapshot/catalog/session identity for read-only projection and
	// exact session recovery. Only the physical client is cleared.
	k.remote = nil
	k.remoteGen = 0
	// Keep remoteTabID, fingerprint, providerAccess, and caches.
	k.mu.Unlock()

	remote := k.targets.Remote()
	errText := "Remote transport closed; reconnecting…"
	state := string(target.StateReconnecting)
	attempt := 1
	retryable := true
	if remote != nil {
		attempt = remote.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		retryable = remote.Retryable
		if remote.ConnState == target.StateReconnectFailed {
			state = string(target.StateReconnectFailed)
			errText = remote.Error
			if errText == "" {
				errText = "Remote reconnect failed"
			}
		}
	}
	a.emitWorkbenchTargetState(state, id, identityGen, requestSeq, errText, attempt, retryable)
	// Do not emit Local runtime rebuilt — Remote remains the projection.
	if remote != nil && remote.ConnState == target.StateReconnecting {
		a.workbenchStartReconnectSupervisor(remote.LogicalGen, remote.Identity.HostID, remote.Identity.Workspace, projectionTabID)
	}
}

func workbenchWireEvent(notification protocol.SessionEvent, tabID string) wireEventTab {
	return wireEventTab{
		Event: notification.Event, TabID: tabID,
		RuntimeEpoch: string(notification.RuntimeEpoch),
	}
}

// remoteRoute classifies the main-window workbench projection for AppBindings.
type remoteRoute int

const (
	remoteRouteLocal remoteRoute = iota
	remoteRouteConnected
	remoteRouteUnavailable // projected Remote with no live physical client
)

func (a *App) remoteRoute() remoteRoute {
	k := a.workbench()
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindRemote {
		return remoteRouteLocal
	}
	if _, _, _, _, ok := a.connectedRemoteWorkbench(); ok {
		return remoteRouteConnected
	}
	return remoteRouteUnavailable
}

// connectedRemoteWorkbench returns the live physical Remote client when the
// projection is Remote and the transport is up.
func (a *App) connectedRemoteWorkbench() (*client.Client, protocol.SessionSnapshot, protocol.WorkspaceCatalogResult, string, bool) {
	k := a.workbench()
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindRemote {
		return nil, protocol.SessionSnapshot{}, protocol.WorkspaceCatalogResult{}, "", false
	}
	remoteState := k.targets.Remote()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.remote == nil || remoteState == nil || !remoteState.Connected || k.remoteGen != remoteState.Generation {
		return nil, protocol.SessionSnapshot{}, protocol.WorkspaceCatalogResult{}, "", false
	}
	return k.remote, k.snapshot, k.catalog, k.remoteTabID, true
}

// projectedRemoteWorkbench returns the cached Remote projection even while
// reconnecting. Mutations must not use this path.
func (a *App) projectedRemoteWorkbench() (protocol.SessionSnapshot, protocol.WorkspaceCatalogResult, string, bool) {
	k := a.workbench()
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindRemote {
		return protocol.SessionSnapshot{}, protocol.WorkspaceCatalogResult{}, "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.snapshot, k.catalog, k.remoteTabID, true
}

// activeRemoteWorkbench is the historical name for the connected Remote client.
// Prefer connectedRemoteWorkbench / projectedRemoteWorkbench / remoteRoute for
// new call sites. Callers that fall through to Local after a false ok MUST first
// ensure the projection is not Remote-unavailable (see remoteMutationBlocked).
func (a *App) activeRemoteWorkbench() (*client.Client, protocol.SessionSnapshot, protocol.WorkspaceCatalogResult, string, bool) {
	return a.connectedRemoteWorkbench()
}

func (a *App) activeWorkbenchTargetIsRemote() bool {
	id, _, _ := a.workbench().targets.Active()
	return id.Kind == target.KindRemote
}

// remoteMutationBlocked returns a Remote reconnecting error when the active
// projection is Remote but the physical client is unavailable. Local execution
// must not run in that state.
func (a *App) remoteMutationBlocked() error {
	if a.remoteRoute() == remoteRouteUnavailable {
		return remoteReconnectingErr()
	}
	return nil
}

func remoteReconnectingErr() error {
	return fmt.Errorf("REMOTE_RECONNECTING: Remote Workbench is reconnecting; try again after the Host session is restored")
}

func remoteSavedSessionManagementErr() error {
	return fmt.Errorf("CAPABILITY_UNAVAILABLE: Remote saved-session management is not available in V1")
}

func remoteMemoryUnavailableErr() error {
	return fmt.Errorf("CAPABILITY_UNAVAILABLE: Remote Memory is not available in V1")
}

func remoteAutoResearchUnavailableErr() error {
	return fmt.Errorf("CAPABILITY_UNAVAILABLE: Remote Auto Research is not available in V1")
}

func (a *App) workbenchSnapshot() (protocol.SessionSnapshot, bool) {
	if snapshot, _, _, ok := a.projectedRemoteWorkbench(); ok {
		return snapshot, true
	}
	return protocol.SessionSnapshot{}, false
}

func (a *App) workbenchProjectTabMetas(metas []TabMeta) []TabMeta {
	snapshot, _, tabID, ok := a.projectedRemoteWorkbench()
	if !ok {
		return metas
	}
	id, _, _ := a.workbench().targets.Active()
	remote := a.workbench().targets.Remote()
	for i := range metas {
		if metas[i].ID != tabID {
			continue
		}
		metas[i].WorkspaceRoot = id.Workspace
		metas[i].WorkspacePath = id.Workspace
		metas[i].WorkspaceName = workbenchBaseName(id.Workspace)
		metas[i].Cwd = id.Workspace
		metas[i].TopicID = string(snapshot.Meta.TopicID)
		metas[i].TopicTitle = snapshot.Meta.Title
		metas[i].Label = snapshot.Meta.ResolvedProfile.Model
		metas[i].Ready = remote != nil && remote.Connected
		metas[i].Runtime = workbenchRuntimeView(snapshot)
		metas[i].Running = snapshot.Runtime.Running
		metas[i].PendingPrompt = snapshot.PendingPrompt != nil
		metas[i].BackgroundJobs = len(snapshot.Jobs)
		metas[i].CancelRequested = snapshot.Runtime.CancelRequested
		// Mutations disabled while reconnecting: not cancellable from Desktop.
		metas[i].Cancellable = remote != nil && remote.Connected &&
			(snapshot.Runtime.CurrentTurn != nil || snapshot.Runtime.CurrentOperation != nil)
		metas[i].Mode = string(snapshot.Meta.ResolvedProfile.CollaborationMode)
		metas[i].CollaborationMode = string(snapshot.Meta.ResolvedProfile.CollaborationMode)
		metas[i].ToolApprovalMode = string(snapshot.Meta.ResolvedProfile.ToolApprovalMode)
		metas[i].TokenMode = string(snapshot.Meta.ResolvedProfile.TokenMode)
		metas[i].Goal = workbenchString(snapshot.Meta.Goal)
		metas[i].GoalStatus = string(snapshot.Meta.GoalStatus)
	}
	return metas
}

// workbenchStartReconnectSupervisor launches automatic exact-session recovery
// for one logical slot. Desktop restarts never restore the supervisor.
func (a *App) workbenchStartReconnectSupervisor(logicalGen uint64, hostID, workspace, tabID string) {
	k := a.workbench()
	k.reconnectMu.Lock()
	defer k.reconnectMu.Unlock()
	k.mu.Lock()
	prev := k.reconnect
	k.reconnect = nil
	k.mu.Unlock()
	if prev != nil {
		prev.Stop()
	}
	sup := reconnect.New(reconnect.Options{}, func(ctx context.Context, attempt int) error {
		return a.workbenchReconnectAttempt(ctx, logicalGen, hostID, workspace, tabID, attempt)
	})
	done, started := sup.StartAsync(a.bootContext())
	if !started {
		return
	}
	k.mu.Lock()
	k.reconnect = sup
	k.mu.Unlock()
	go func() {
		<-done
		// Only the still-registered supervisor may mark window expiry. Explicit
		// disconnect / replacement clears k.reconnect before Stop returns.
		k := a.workbench()
		k.mu.Lock()
		stillMine := k.reconnect == sup
		if stillMine {
			k.reconnect = nil
		}
		k.mu.Unlock()
		if !stillMine {
			return
		}
		remote := k.targets.Remote()
		if remote == nil || remote.LogicalGen != logicalGen {
			return
		}
		if remote.Connected && remote.ConnState == target.StateConnected {
			return
		}
		if remote.ConnState == target.StateReconnectFailed {
			return
		}
		id, gen, seq, ok := k.targets.MarkReconnectFailed(logicalGen, "automatic reconnect window expired")
		if ok {
			a.emitWorkbenchTargetState(string(target.StateReconnectFailed), id, gen, seq, "automatic reconnect window expired", remote.Attempt, true)
		}
	}()
}

func (a *App) workbenchStopReconnectSupervisor() {
	k := a.workbench()
	k.reconnectMu.Lock()
	defer k.reconnectMu.Unlock()
	k.mu.Lock()
	sup := k.reconnect
	k.reconnect = nil
	k.mu.Unlock()
	if sup != nil {
		sup.Stop()
	}
}

// workbenchReconnectAttempt performs one failure-atomic candidate reconnect for
// the saved exact RuntimeTarget. On success it commits under the transition fence.
func (a *App) workbenchReconnectAttempt(ctx context.Context, logicalGen uint64, hostID, workspace, tabID string, attempt int) error {
	k := a.workbench()
	remote := k.targets.Remote()
	if remote == nil || remote.LogicalGen != logicalGen {
		return reconnect.ErrStaleSlot
	}
	if remote.Identity.HostID != hostID || remote.Identity.Workspace != workspace {
		return reconnect.ErrStaleSlot
	}
	k.targets.UpdateReconnectAttempt(logicalGen, attempt, "")
	id, gen, seq := k.targets.Active()
	a.emitWorkbenchTargetState(string(target.StateReconnecting), id, gen, seq, reconnect.FormatAttemptError(attempt, nil), attempt, true)

	// Reuse the full connect path with exact-session recovery. Manual connect
	// and auto-reconnect share workbenchConnectRemoteCore.
	err := a.workbenchConnectRemoteCore(ctx, hostID, workspace, workbenchConnectOptions{
		LogicalGen:          logicalGen,
		ExactTarget:         remote.Target,
		ExactEpoch:          remote.RuntimeEpoch,
		RequireExact:        remote.Target.SessionID != "",
		ExpectedFingerprint: remote.Fingerprint,
		SkipProviderTrust:   true, // auto-reconnect must not re-prompt trust
		IsAutoReconnect:     true,
		Attempt:             attempt,
	})
	if err != nil {
		// Supervisor cancellation and logical-slot replacement are lifecycle
		// fences, not reconnect failures. Do not overwrite a newer/manual state.
		if ctx.Err() != nil || errors.Is(err, reconnect.ErrStaleSlot) {
			return err
		}
		class := reconnect.ClassifyError(err)
		k.targets.UpdateReconnectAttempt(logicalGen, attempt, err.Error())
		id, gen, seq := k.targets.Active()
		a.emitWorkbenchTargetState(string(target.StateReconnecting), id, gen, seq, reconnect.FormatAttemptError(attempt, err), attempt, true)
		if class == reconnect.ClassStop {
			id, gen, seq, changed := k.targets.MarkReconnectFailed(logicalGen, err.Error())
			if changed {
				a.emitWorkbenchTargetState(string(target.StateReconnectFailed), id, gen, seq, err.Error(), attempt, true)
			}
		}
		return err
	}
	return nil
}

func workbenchRuntimeView(snapshot protocol.SessionSnapshot) SessionRuntimeView {
	return SessionRuntimeView{Phase: sessionRuntimeReady, Epoch: string(snapshot.RuntimeEpoch)}
}

func workbenchBaseName(path string) string {
	path = strings.TrimRight(strings.ReplaceAll(path, "\\", "/"), "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func workbenchLoadCatalog(ctx context.Context, cli *client.Client) (protocol.WorkspaceCatalogResult, error) {
	raw, err := cli.Request(ctx, string(protocol.MethodCatalogWorkspace), protocol.WorkspaceCatalogParams{})
	if err != nil {
		return protocol.WorkspaceCatalogResult{}, err
	}
	decoded, err := protocol.DecodeResult(protocol.MethodCatalogWorkspace, raw)
	if err != nil {
		return protocol.WorkspaceCatalogResult{}, err
	}
	return decoded.(protocol.WorkspaceCatalogResult), nil
}

func workbenchLoadSessionCatalog(ctx context.Context, cli *client.Client) (protocol.SessionCatalogResult, error) {
	raw, err := cli.Request(ctx, string(protocol.MethodCatalogSession), protocol.SessionCatalogParams{})
	if err != nil {
		return protocol.SessionCatalogResult{}, err
	}
	decoded, err := protocol.DecodeResult(protocol.MethodCatalogSession, raw)
	if err != nil {
		return protocol.SessionCatalogResult{}, err
	}
	return decoded.(protocol.SessionCatalogResult), nil
}

func (a *App) workbenchSessionCatalog() (protocol.SessionCatalogResult, bool) {
	k := a.workbench()
	id, _, _ := k.targets.Active()
	if id.Kind != target.KindRemote {
		return protocol.SessionCatalogResult{}, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sessionCatalog, true
}

func (a *App) workbenchRefreshCatalog(generation uint64) {
	cli, _, _, _, ok := a.activeRemoteWorkbench()
	if !ok || cli.Generation() != generation {
		return
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 10*time.Second)
	defer cancel()
	catalog, err := workbenchLoadCatalog(ctx, cli)
	if err != nil {
		return
	}
	sessionCatalog, err := workbenchLoadSessionCatalog(ctx, cli)
	if err != nil {
		return
	}
	k := a.workbench()
	k.mu.Lock()
	if k.remote == cli && k.remoteGen == generation {
		k.catalog = catalog
		k.sessionCatalog = sessionCatalog
	}
	k.mu.Unlock()
}

func (a *App) workbenchRefreshSnapshot(generation uint64, tabID string) {
	cli, _, _, _, ok := a.activeRemoteWorkbench()
	if !ok || cli.Generation() != generation {
		return
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 15*time.Second)
	defer cancel()
	_, err := cli.SubscribeCommitted(ctx, protocol.HistoryMaxTurns, func(result protocol.SessionSubscribeResult) error {
		k := a.workbench()
		k.transitionMu.Lock()
		defer k.transitionMu.Unlock()
		active, _, _ := k.targets.Active()
		if active.Kind != target.KindRemote {
			return nil
		}
		k.mu.Lock()
		previous := k.snapshot
		if k.remote == cli && k.remoteGen == generation {
			k.snapshot = result.Snapshot
			if k.remoteTabID != "" {
				tabID = k.remoteTabID
			}
		} else {
			k.mu.Unlock()
			return nil
		}
		k.mu.Unlock()
		go a.workbenchMirrorSnapshot(cli, result.Snapshot)
		a.emitReady(a.ctx, tabID)
		if workbenchSnapshotRuntimeReplaced(previous, result.Snapshot) {
			a.emitWorkbenchRuntimeRebuilt(tabID, string(result.Snapshot.RuntimeEpoch))
		}
		return nil
	})
	if err != nil {
		return
	}
}

// workbenchSnapshotRuntimeReplaced distinguishes an actual Remote controller
// replacement from an ordinary state refresh. Treating every subscribe as a
// rebuild invalidates frontend generations and can feed profile synchronization
// back into session/profile/set indefinitely.
func workbenchSnapshotRuntimeReplaced(previous, next protocol.SessionSnapshot) bool {
	if previous.Target.SessionID == "" || previous.RuntimeEpoch == "" {
		return false
	}
	return previous.Target != next.Target || previous.RuntimeEpoch != next.RuntimeEpoch
}

func workbenchSnapshotMatchesProfileResult(snapshot protocol.SessionSnapshot, target protocol.RuntimeTarget, patch protocol.ProfilePatch, result protocol.SessionProfileSetResult) bool {
	if snapshot.Target != target || snapshot.RuntimeEpoch != result.RuntimeEpoch || snapshot.Meta.ResolvedProfile != result.ResolvedProfile {
		return false
	}
	if patch.Goal == nil {
		return true
	}
	wantGoal := strings.TrimSpace(*patch.Goal)
	gotGoal := ""
	if snapshot.Meta.Goal != nil {
		gotGoal = strings.TrimSpace(*snapshot.Meta.Goal)
	}
	return gotGoal == wantGoal
}

func (a *App) workbenchMirrorSnapshot(cli *client.Client, snapshot protocol.SessionSnapshot) {
	var artifact *protocol.ExternalizedField
	for i := range snapshot.Externalized {
		if snapshot.Externalized[i].JSONPointer == "/mirror/session.jsonl" {
			copy := snapshot.Externalized[i]
			artifact = &copy
			break
		}
	}
	if artifact == nil || artifact.TotalBytes <= 0 || artifact.TotalBytes > protocol.ContentRefObjectBytes {
		return
	}
	k := a.workbench()
	k.mu.Lock()
	if k.remote != cli {
		k.mu.Unlock()
		return
	}
	fingerprint := k.remoteFingerprint
	k.mu.Unlock()
	id, _, _ := k.targets.Active()
	if fingerprint == "" || id.Kind != target.KindRemote || id.Workspace == "" {
		return
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 30*time.Second)
	defer cancel()
	data := make([]byte, 0, artifact.TotalBytes)
	var offset int64
	for {
		raw, err := cli.Request(ctx, string(protocol.MethodSessionContent), protocol.SessionContentParams{ContentRef: artifact.ContentRef, Offset: offset})
		if err != nil {
			return
		}
		decoded, err := protocol.DecodeResult(protocol.MethodSessionContent, raw)
		if err != nil {
			return
		}
		chunk := decoded.(protocol.SessionContentResult)
		bytes, err := base64.StdEncoding.DecodeString(chunk.DataBase64)
		if err != nil {
			return
		}
		data = append(data, bytes...)
		if chunk.NextOffset == nil {
			break
		}
		offset = *chunk.NextOffset
	}
	if int64(len(data)) != artifact.TotalBytes {
		return
	}
	sum := sha256.Sum256(data)
	contentSHA := hex.EncodeToString(sum[:])
	if contentSHA != artifact.SHA256 {
		return
	}
	artifacts := map[string][]byte{"session.jsonl": data}
	_ = (mirror.Store{}).ApplyCheckpoint(fingerprint, id.Workspace, mirror.Manifest{
		SessionID: string(snapshot.Target.SessionID), Revision: time.Now().UnixMilli(),
		Digest: mirror.DigestArtifacts(artifacts), ModelRef: snapshot.Meta.ResolvedProfile.Model,
		Label: snapshot.Meta.Title, ArtifactSHA: map[string]string{"session.jsonl": contentSHA},
	}, artifacts)
}

func (a *App) workbenchRequest(method protocol.Method, params any) ([]byte, error) {
	if err := a.remoteMutationBlocked(); err != nil {
		return nil, err
	}
	cli, _, _, _, ok := a.activeRemoteWorkbench()
	if !ok {
		if a.activeWorkbenchTargetIsRemote() {
			return nil, remoteReconnectingErr()
		}
		return nil, fmt.Errorf("CAPABILITY_UNAVAILABLE: active target is local")
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 30*time.Second)
	defer cancel()
	return cli.Request(ctx, string(method), params)
}

func (a *App) workbenchSubmit(input, display, editedOriginal string, invocations []protocol.Invocation, recovery bool) (bool, error) {
	if a.workbench().targets.Connecting() {
		return true, fmt.Errorf("Remote target is connecting; wait for the connection to finish")
	}
	if a.activeWorkbenchTargetIsRemote() {
		cli, _, _, _, ok := a.activeRemoteWorkbench()
		if !ok {
			// Never fall through to Local while Remote is the projection.
			return true, remoteReconnectingErr()
		}
		input = strings.TrimSpace(input)
		if input == "" {
			return true, fmt.Errorf("input is required")
		}
		ctx, cancel := context.WithTimeout(a.bootContext(), 30*time.Second)
		defer cancel()
		_, err := cli.Request(ctx, string(protocol.MethodSessionSubmit), protocol.SessionSubmitParams{
			Input: input, DisplayText: display, EditedOriginal: editedOriginal,
			Invocations: invocations, DeliveryRecovery: recovery,
		})
		return true, err
	}
	return false, nil
}

func (a *App) workbenchSetProfile(patch protocol.ProfilePatch) (bool, error) {
	if !a.activeWorkbenchTargetIsRemote() {
		return false, nil
	}
	cli, snapshot, _, tabID, ok := a.activeRemoteWorkbench()
	if !ok {
		return true, remoteReconnectingErr()
	}
	ctx, cancel := context.WithTimeout(a.bootContext(), 30*time.Second)
	defer cancel()
	result, err := cli.SetProfile(ctx, patch)
	if err != nil {
		return true, err
	}
	if result.Disposition == protocol.ProfileUnchanged && workbenchSnapshotMatchesProfileResult(snapshot, cli.State().Target, patch, result) {
		return true, nil
	}
	a.workbenchRefreshSnapshot(cli.Generation(), tabID)
	return true, nil
}
