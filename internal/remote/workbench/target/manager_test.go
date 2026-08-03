package target

import (
	"testing"

	"reasonix/internal/remote/protocol"
)

func TestManagerStartsLocalAndFencesSwitch(t *testing.T) {
	m := New()
	id, gen, seq := m.Active()
	if id.Kind != KindLocal || gen == 0 {
		t.Fatalf("active = %+v gen=%d", id, gen)
	}

	remoteID, rgen, err := m.BeginRemoteConnect("lab", "/home/u/w")
	if err != nil {
		t.Fatal(err)
	}
	if remoteID.HostID != "lab" {
		t.Fatalf("remote = %+v", remoteID)
	}
	if !m.Connecting() {
		t.Fatal("candidate connection was not reported")
	}
	if err := m.MarkRemoteConnected(rgen); err != nil {
		t.Fatal(err)
	}
	active, igen, rseq, err := m.ActivateRemote(rgen)
	if err != nil {
		t.Fatal(err)
	}
	if active.Kind != KindRemote {
		t.Fatalf("active = %+v", active)
	}
	if m.Connecting() {
		t.Fatal("committed connection remained marked as connecting")
	}
	// Old tokens are stale after activate.
	if !m.IsStale(gen, seq) {
		t.Fatal("expected pre-switch tokens to be stale")
	}
	if m.IsStale(igen, rseq) {
		t.Fatal("current tokens should not be stale")
	}

	// Switch back to local while remote stays connected in background.
	m.SwitchLocal()
	bg := m.RemoteBackground()
	if bg == nil || !bg.Connected {
		t.Fatalf("background remote = %+v", bg)
	}
	hint := m.LastRemoteHint()
	if hint.HostID != "lab" || hint.Workspace != "/home/u/w" {
		t.Fatalf("hint = %+v", hint)
	}
	// Returning to the same connected adapter is allowed even if it became busy
	// in the background; this does not replace the Host or transport.
	m.SetRemoteBusy(true)
	if active, _, _, err := m.ActivateRemote(rgen); err != nil || active.Kind != KindRemote {
		t.Fatalf("reactivate connected remote = %+v err=%v", active, err)
	}
}

func TestManagerRejectsBusyHostSwap(t *testing.T) {
	m := New()
	_, gen, err := m.BeginRemoteConnect("a", "/w1")
	if err != nil {
		t.Fatal(err)
	}
	_ = m.MarkRemoteConnected(gen)
	m.SetRemoteBusy(true)
	if _, _, err := m.BeginRemoteConnect("b", "/w2"); err == nil {
		t.Fatal("expected busy rejection")
	}
	if err := m.DetachRemote(); err == nil {
		t.Fatal("expected busy detach rejection")
	}
	m.SetRemoteBusy(false)
	if err := m.DetachRemote(); err != nil {
		t.Fatal(err)
	}
	id, _, _ := m.Active()
	if id.Kind != KindLocal {
		t.Fatalf("after detach active = %+v", id)
	}
}

func TestManagerSeparatesAttachAndProjectionGenerations(t *testing.T) {
	m := New()
	_, attachGen, err := m.BeginRemoteConnect("lab", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRemoteConnected(attachGen); err != nil {
		t.Fatal(err)
	}
	_, projectionGen, _, err := m.ActivateRemote(attachGen)
	if err != nil {
		t.Fatal(err)
	}
	if projectionGen == attachGen {
		t.Fatal("projection generation must fence the preceding connect transition")
	}
	remote := m.Remote()
	if remote == nil || remote.Generation != attachGen || !remote.Connected {
		t.Fatalf("remote lifecycle = %+v, want attach generation %d", remote, attachGen)
	}
	if m.AbortRemoteConnect(attachGen) {
		t.Fatal("abort unexpectedly cleared the committed remote")
	}
	if m.Remote() == nil {
		t.Fatal("committed remote was lost after stale abort")
	}
}

func TestFailedReplacementPreservesCommittedRemote(t *testing.T) {
	m := New()
	_, firstGen, err := m.BeginRemoteConnect("host-a", "/workspace-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRemoteConnected(firstGen); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.ActivateRemote(firstGen); err != nil {
		t.Fatal(err)
	}
	_, activeGenBefore, requestSeqBefore := m.Active()

	_, replacementGen, err := m.BeginRemoteConnect("host-b", "/workspace-b")
	if err != nil {
		t.Fatal(err)
	}
	committed := m.Remote()
	if committed == nil || committed.Identity.HostID != "host-a" || committed.Generation != firstGen {
		t.Fatalf("replacement attempt changed committed remote: %+v", committed)
	}
	if !m.AbortRemoteConnect(replacementGen) {
		t.Fatal("failed replacement was not aborted")
	}
	committed = m.Remote()
	active, activeGenAfter, requestSeqAfter := m.Active()
	if committed == nil || committed.Identity.HostID != "host-a" || active.HostID != "host-a" {
		t.Fatalf("failed replacement lost active remote: committed=%+v active=%+v", committed, active)
	}
	if activeGenAfter != activeGenBefore || requestSeqAfter != requestSeqBefore {
		t.Fatalf("failed replacement fenced the committed projection: before=(%d,%d) after=(%d,%d)", activeGenBefore, requestSeqBefore, activeGenAfter, requestSeqAfter)
	}
}

func TestSuccessfulReplacementPromotesAtomically(t *testing.T) {
	m := New()
	_, firstGen, _ := m.BeginRemoteConnect("host-a", "/workspace-a")
	_ = m.MarkRemoteConnected(firstGen)
	_, _, _, _ = m.ActivateRemote(firstGen)

	_, replacementGen, err := m.BeginRemoteConnect("host-b", "/workspace-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRemoteConnected(replacementGen); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.ActivateRemote(replacementGen); err != nil {
		t.Fatal(err)
	}
	committed := m.Remote()
	if committed == nil || committed.Identity.HostID != "host-b" || committed.Generation != replacementGen {
		t.Fatalf("replacement was not promoted: %+v", committed)
	}
}

func TestManagerUnexpectedDisconnectKeepsRemoteProjection(t *testing.T) {
	m := New()
	_, attachGen, err := m.BeginRemoteConnect("lab", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkRemoteConnected(attachGen); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.ActivateRemote(attachGen); err != nil {
		t.Fatal(err)
	}
	m.SetRemoteSession(attachGen, protocol.RuntimeTarget{WorkspaceID: "ws", SessionID: "sess"}, "epoch-1", "fp")
	m.SetRemoteBusy(true)
	id, _, _, changed := m.MarkRemoteDisconnected(attachGen)
	if !changed || id.Kind != KindRemote {
		t.Fatalf("disconnect = %+v changed=%v, want keep Remote projection", id, changed)
	}
	remote := m.Remote()
	if remote == nil || remote.Connected || remote.ConnState != StateReconnecting {
		t.Fatalf("remote lifecycle = %+v", remote)
	}
	if remote.Target.SessionID != "sess" || remote.RuntimeEpoch != "epoch-1" {
		t.Fatalf("exact session not preserved: %+v", remote)
	}
	if !m.ProjectedRemote() || m.ConnectedRemote() {
		t.Fatal("projected remote should be true and connected false")
	}
	// Manual retry of the same slot is allowed after busy was cleared by disconnect.
	if _, _, err := m.BeginRemoteConnect("lab", "/work"); err != nil {
		t.Fatalf("reconnect remained fenced as busy: %v", err)
	}
	if _, _, _, changed := m.MarkRemoteDisconnected(attachGen); changed {
		t.Fatal("stale disconnect changed replacement connection")
	}
}

func TestManagerLatePhysicalCallbackIgnored(t *testing.T) {
	m := New()
	_, firstGen, _ := m.BeginRemoteConnect("lab", "/work")
	_ = m.MarkRemoteConnected(firstGen)
	_, _, _, _ = m.ActivateRemote(firstGen)
	_, _, _, _ = m.MarkRemoteDisconnected(firstGen)

	phys := m.NextAttachGeneration()
	if err := m.BeginAutoReconnect(m.Remote().LogicalGen, phys, 2); err != nil {
		t.Fatal(err)
	}
	_ = m.MarkRemoteConnected(phys)
	if _, _, _, err := m.ActivateRemote(phys); err != nil {
		t.Fatal(err)
	}
	// Late disconnect from the first physical generation must not tear down the winner.
	if _, _, _, changed := m.MarkRemoteDisconnected(firstGen); changed {
		t.Fatal("late disconnect affected winner")
	}
	if remote := m.Remote(); remote == nil || !remote.Connected || remote.Generation != phys {
		t.Fatalf("winner lost: %+v", remote)
	}
}

func TestManagerReconnectFailedPreservesSlot(t *testing.T) {
	m := New()
	_, gen, _ := m.BeginRemoteConnect("lab", "/work")
	_ = m.MarkRemoteConnected(gen)
	_, _, _, _ = m.ActivateRemote(gen)
	m.SetRemoteSession(gen, protocol.RuntimeTarget{WorkspaceID: "ws", SessionID: "sess"}, "epoch-1", "fp")
	_, _, _, _ = m.MarkRemoteDisconnected(gen)
	logical := m.Remote().LogicalGen
	id, _, _, ok := m.MarkReconnectFailed(logical, "window expired")
	if !ok || id.Kind != KindRemote {
		t.Fatalf("failed = %+v ok=%v", id, ok)
	}
	remote := m.Remote()
	if remote.ConnState != StateReconnectFailed || remote.Target.SessionID != "sess" {
		t.Fatalf("remote = %+v", remote)
	}
	// Manual retry continues the exact target.
	_, retryGen, err := m.BeginRemoteConnect("lab", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if m.connecting == nil || m.connecting.Target.SessionID != "sess" {
		t.Fatalf("manual retry lost target: %+v", m.connecting)
	}
	_ = retryGen
}

func TestCommitBackgroundRemoteDoesNotStealProjection(t *testing.T) {
	m := New()
	_, gen, _ := m.BeginRemoteConnect("lab", "/work")
	_ = m.MarkRemoteConnected(gen)
	_, _, _, _ = m.ActivateRemote(gen)
	m.SwitchLocal()
	_, localGen, localSeq := m.Active()
	_, disconnectedGen, disconnectedSeq, _ := m.MarkRemoteDisconnected(gen)
	if disconnectedGen != localGen || disconnectedSeq != localSeq {
		t.Fatalf("background disconnect invalidated Local token: before=(%d,%d) after=(%d,%d)", localGen, localSeq, disconnectedGen, disconnectedSeq)
	}

	phys := m.NextAttachGeneration()
	if err := m.BeginAutoReconnect(m.Remote().LogicalGen, phys, 1); err != nil {
		t.Fatal(err)
	}
	_ = m.MarkRemoteConnected(phys)
	_, beforeGen, beforeSeq := m.Active()
	active, afterGen, afterSeq, err := m.CommitBackgroundRemote(phys)
	if err != nil {
		t.Fatal(err)
	}
	if active.Kind != KindLocal {
		t.Fatalf("background commit stole projection: %+v", active)
	}
	if afterGen != beforeGen || afterSeq != beforeSeq {
		t.Fatalf("background commit invalidated Local token: before=(%d,%d) after=(%d,%d)", beforeGen, beforeSeq, afterGen, afterSeq)
	}
	if remote := m.Remote(); remote == nil || !remote.Connected {
		t.Fatalf("background remote not connected: %+v", remote)
	}
}
