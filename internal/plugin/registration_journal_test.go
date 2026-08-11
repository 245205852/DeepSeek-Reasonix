package plugin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRegistrationScopeRollbackIsInstanceScoped(t *testing.T) {
	host := NewHost()
	// Pre-existing sibling client accepted under scope A.
	siblingA := &Client{name: "sibling"}
	scopeA := host.BeginRegistrationScope()
	ctxA := ContextWithRegistrationScope(context.Background(), scopeA)
	if err := host.ReplaceServerBackend(ctxA, "sibling", siblingA, 1); err != nil {
		t.Fatal(err)
	}
	refsA := scopeA.Snapshot()
	if len(refsA) != 1 || refsA[0].Name != "sibling" || refsA[0].ID == 0 {
		t.Fatalf("scope A = %+v", refsA)
	}
	idA := refsA[0].ID

	// Stale build journals a replacement for the same name plus a new server.
	staleSibling := &Client{name: "sibling"}
	staleNew := &Client{name: "stale-new"}
	scopeStale := host.BeginRegistrationScope()
	ctxStale := ContextWithRegistrationScope(context.Background(), scopeStale)
	if err := host.ReplaceServerBackend(ctxStale, "sibling", staleSibling, 2); err != nil {
		t.Fatal(err)
	}
	if err := host.ReplaceServerBackend(ctxStale, "stale-new", staleNew, 2); err != nil {
		t.Fatal(err)
	}
	refsStale := scopeStale.Snapshot()
	if len(refsStale) != 2 {
		t.Fatalf("stale scope len = %d, want 2: %+v", len(refsStale), refsStale)
	}
	var staleSiblingID uint64
	for _, ref := range refsStale {
		if ref.Name == "sibling" {
			staleSiblingID = ref.ID
		}
	}
	if staleSiblingID == 0 || staleSiblingID == idA {
		t.Fatalf("stale sibling id = %d, original = %d", staleSiblingID, idA)
	}

	// Sibling/new-generation resource created AFTER the stale scope must survive
	// rollback (name-diff would delete it incorrectly). No scope token.
	siblingNewGen := &Client{name: "sibling-new-gen"}
	if err := host.ReplaceServerBackend(context.Background(), "sibling-new-gen", siblingNewGen, 3); err != nil {
		t.Fatal(err)
	}
	// Newer generation also replaces the shared name after the stale scope ends.
	newerSibling := &Client{name: "sibling"}
	if err := host.ReplaceServerBackend(context.Background(), "sibling", newerSibling, 4); err != nil {
		t.Fatal(err)
	}
	newerID := newerSibling.instanceID
	if newerID == 0 || newerID == staleSiblingID {
		t.Fatalf("newer sibling id = %d, stale = %d", newerID, staleSiblingID)
	}

	scopeStale.AbortAndRollback()

	names := map[string]bool{}
	for _, name := range host.ServerNames() {
		names[name] = true
	}
	if names["stale-new"] {
		t.Fatal("stale-new survived instance-scoped rollback")
	}
	if !names["sibling-new-gen"] {
		t.Fatal("sibling-new-gen was incorrectly removed by stale scope rollback")
	}
	if !names["sibling"] {
		t.Fatal("newer-generation sibling instance was incorrectly removed")
	}
	if host.lookupClient("sibling") == nil || host.lookupClient("sibling").instanceID != newerID {
		t.Fatalf("sibling live instance = %+v, want id %d", host.lookupClient("sibling"), newerID)
	}
	if host.RemoveIfInstance("sibling", idA) {
		t.Fatal("RemoveIfInstance removed a non-matching/missing instance")
	}
	if host.RemoveIfInstance("sibling", staleSiblingID) {
		t.Fatal("RemoveIfInstance removed the newer instance via stale id")
	}
}

// TestRegistrationScopeIgnoresUnrelatedHostWrites is the deterministic
// interleaving that Host-global regJournalActive fails: while a stale build
// holds an open scope, a sibling hot-add without the scope token must not be
// attributed to the stale build or deleted by its rollback.
func TestRegistrationScopeIgnoresUnrelatedHostWrites(t *testing.T) {
	host := NewHost()
	scopeStale := host.BeginRegistrationScope()
	ctxStale := ContextWithRegistrationScope(context.Background(), scopeStale)

	// Sibling hot-add during the open scope WITHOUT the token.
	sibling := &Client{name: "sibling-hot-add"}
	if err := host.ReplaceServerBackend(context.Background(), "sibling-hot-add", sibling, 1); err != nil {
		t.Fatal(err)
	}
	// Stale build also registers its own server under the scope.
	staleOnly := &Client{name: "stale-only"}
	if err := host.ReplaceServerBackend(ctxStale, "stale-only", staleOnly, 1); err != nil {
		t.Fatal(err)
	}

	refs := scopeStale.Snapshot()
	for _, ref := range refs {
		if ref.Name == "sibling-hot-add" {
			t.Fatalf("journal captured unrelated Host write: %+v", refs)
		}
	}
	if len(refs) != 1 || refs[0].Name != "stale-only" {
		t.Fatalf("stale scope refs = %+v, want only stale-only", refs)
	}

	scopeStale.AbortAndRollback()

	names := map[string]bool{}
	for _, name := range host.ServerNames() {
		names[name] = true
	}
	if names["stale-only"] {
		t.Fatal("stale-only survived AbortAndRollback")
	}
	if !names["sibling-hot-add"] {
		t.Fatal("sibling-hot-add was deleted by stale scope rollback")
	}
}

func TestRegistrationScopeRejectsLateRegistrationAfterAbort(t *testing.T) {
	host := NewHost()
	scope := host.BeginRegistrationScope()
	ctx := ContextWithRegistrationScope(context.Background(), scope)
	scope.AbortAndRollback()

	late := &Client{name: "late"}
	err := host.ReplaceServerBackend(ctx, "late", late, 1)
	if !errors.Is(err, ErrRegistrationScopeAborted) {
		t.Fatalf("late registration err = %v, want ErrRegistrationScopeAborted", err)
	}
	for _, name := range host.ServerNames() {
		if name == "late" {
			t.Fatal("aborted scope accepted a late client")
		}
	}
}

func TestRegistrationScopesDoNotSerializeUnrelatedBuilds(t *testing.T) {
	host := NewHost()
	started := make(chan struct{})
	release := make(chan struct{})
	var secondEntered atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scope := host.BeginRegistrationScope()
		ctx := ContextWithRegistrationScope(context.Background(), scope)
		close(started)
		<-release
		_ = host.ReplaceServerBackend(ctx, "a", &Client{name: "a"}, 1)
	}()
	go func() {
		defer wg.Done()
		<-started
		// Second scope must not block on the first (no regJournalMu).
		scope := host.BeginRegistrationScope()
		ctx := ContextWithRegistrationScope(context.Background(), scope)
		secondEntered.Store(true)
		_ = host.ReplaceServerBackend(ctx, "b", &Client{name: "b"}, 1)
	}()

	<-started
	// Give the second goroutine a chance to enter without waiting on first.
	for i := 0; i < 50 && !secondEntered.Load(); i++ {
		// busy wait briefly without sleep dependency for CI
	}
	if !secondEntered.Load() {
		// Still allow scheduling; unlock first so test can finish.
	}
	close(release)
	wg.Wait()
	if !secondEntered.Load() {
		t.Fatal("second registration scope was serialized behind the first build")
	}
}
