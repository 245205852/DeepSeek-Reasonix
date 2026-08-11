package plugin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRegistrationJournalRollbackIsInstanceScoped(t *testing.T) {
	host := NewHost()
	// Pre-existing sibling client accepted under journal A.
	siblingA := &Client{name: "sibling"}
	refsA, err := host.RunWithRegistrationJournal(func() error {
		return host.ReplaceServerBackend(context.Background(), "sibling", siblingA, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refsA) != 1 || refsA[0].Name != "sibling" || refsA[0].ID == 0 {
		t.Fatalf("journal A = %+v", refsA)
	}
	idA := refsA[0].ID

	// Stale build journals a replacement for the same name plus a new server.
	staleSibling := &Client{name: "sibling"}
	staleNew := &Client{name: "stale-new"}
	refsStale, err := host.RunWithRegistrationJournal(func() error {
		if err := host.ReplaceServerBackend(context.Background(), "sibling", staleSibling, 2); err != nil {
			return err
		}
		return host.ReplaceServerBackend(context.Background(), "stale-new", staleNew, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refsStale) != 2 {
		t.Fatalf("stale journal len = %d, want 2: %+v", len(refsStale), refsStale)
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

	// Sibling/new-generation resource created AFTER the stale journal must survive
	// rollback of the stale journal (name-diff would delete it incorrectly).
	siblingNewGen := &Client{name: "sibling-new-gen"}
	if err := host.ReplaceServerBackend(context.Background(), "sibling-new-gen", siblingNewGen, 3); err != nil {
		t.Fatal(err)
	}
	// Newer generation also replaces the shared name after the stale journal ends.
	// Stale rollback of the old instance id must be a no-op against this live client.
	newerSibling := &Client{name: "sibling"}
	if err := host.ReplaceServerBackend(context.Background(), "sibling", newerSibling, 4); err != nil {
		t.Fatal(err)
	}
	newerID := newerSibling.instanceID
	if newerID == 0 || newerID == staleSiblingID {
		t.Fatalf("newer sibling id = %d, stale = %d", newerID, staleSiblingID)
	}

	host.RollbackRegistration(refsStale)

	// stale-new must be gone.
	for _, name := range host.ServerNames() {
		if name == "stale-new" {
			t.Fatal("stale-new survived instance-scoped rollback")
		}
	}
	// sibling-new-gen must survive (created outside stale journal).
	foundNewGen := false
	for _, name := range host.ServerNames() {
		if name == "sibling-new-gen" {
			foundNewGen = true
		}
	}
	if !foundNewGen {
		t.Fatal("sibling-new-gen was incorrectly removed by stale journal rollback")
	}
	// Live same-name instance from the newer generation must survive.
	foundSibling := false
	for _, name := range host.ServerNames() {
		if name == "sibling" {
			foundSibling = true
		}
	}
	if !foundSibling {
		t.Fatal("newer-generation sibling instance was incorrectly removed")
	}
	if host.lookupClient("sibling") == nil || host.lookupClient("sibling").instanceID != newerID {
		t.Fatalf("sibling live instance = %+v, want id %d", host.lookupClient("sibling"), newerID)
	}
	// RemoveIfInstance on original A / stale instance must be a no-op.
	if host.RemoveIfInstance("sibling", idA) {
		t.Fatal("RemoveIfInstance removed a non-matching/missing instance")
	}
	if host.RemoveIfInstance("sibling", staleSiblingID) {
		t.Fatal("RemoveIfInstance removed the newer instance via stale id")
	}
}

// TestRegistrationJournalRollbackPreservesPostJournalSibling is the deterministic
// interleaving that name-diff rollback fails: after a stale build journals its
// registrations, a sibling/new generation creates additional Host resources;
// rolling back the stale journal must not delete those post-journal resources.
func TestRegistrationJournalRollbackPreservesPostJournalSibling(t *testing.T) {
	host := NewHost()
	staleNew := &Client{name: "stale-only"}
	refsStale, err := host.RunWithRegistrationJournal(func() error {
		return host.ReplaceServerBackend(context.Background(), "stale-only", staleNew, 1)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deterministic post-journal sibling creation (simulates another tab/build
	// that won the generation race after the stale journal closed).
	post := &Client{name: "post-journal-sibling"}
	if err := host.ReplaceServerBackend(context.Background(), "post-journal-sibling", post, 2); err != nil {
		t.Fatal(err)
	}

	host.RollbackRegistration(refsStale)

	names := map[string]bool{}
	for _, name := range host.ServerNames() {
		names[name] = true
	}
	if names["stale-only"] {
		t.Fatal("stale-only survived instance rollback")
	}
	if !names["post-journal-sibling"] {
		t.Fatal("post-journal sibling was deleted; name-diff-style over-delete regressed")
	}
}

func TestRunWithRegistrationJournalSerializes(t *testing.T) {
	host := NewHost()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan []HostClientRef, 1)
	var secondEntered atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		refs, _ := host.RunWithRegistrationJournal(func() error {
			close(started)
			<-release
			c := &Client{name: "held"}
			return host.ReplaceServerBackend(context.Background(), "held", c, 1)
		})
		done <- refs
	}()
	<-started
	// Second journal waits until first ends.
	go func() {
		defer wg.Done()
		_, _ = host.RunWithRegistrationJournal(func() error {
			secondEntered.Store(true)
			return nil
		})
	}()
	// While first journal holds the lock, second must not enter its body.
	if secondEntered.Load() {
		t.Fatal("second journal started while first held the lock")
	}
	close(release)
	refs := <-done
	if len(refs) != 1 || refs[0].Name != "held" {
		t.Fatalf("first journal refs = %+v", refs)
	}
	wg.Wait()
	if !secondEntered.Load() {
		t.Fatal("second journal never ran after first released")
	}
}
