package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/provider"
	"reasonix/internal/sessioncatalog"
)

func TestGetRecoveryLineageIncludesOriginalAndUserFacingMetadata(t *testing.T) {
	dir := t.TempDir()
	catalog, err := sessioncatalog.Open(context.Background(), sessioncatalog.Options{InMemory: true, DisableRepair: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close(context.Background()) })
	root := filepath.Join(dir, "root.jsonl")
	branch := filepath.Join(dir, "branch.jsonl")
	save := func(path string, messages ...string) {
		t.Helper()
		session := agent.NewSession("system")
		for index, message := range messages {
			role := provider.RoleUser
			if index%2 == 1 {
				role = provider.RoleAssistant
			}
			session.Add(provider.Message{Role: role, Content: message})
		}
		if err := session.Save(path); err != nil {
			t.Fatal(err)
		}
	}
	save(root, "shared question", "shared answer", "original preview", "original answer")
	save(branch, "shared question", "shared answer", "branch preview", "branch answer")
	created := time.UnixMilli(10)
	for path, meta := range map[string]agent.BranchMeta{
		root: {
			ID: "root", Scope: "global", TopicID: "topic", TopicTitle: "Topic",
			CustomTitle: "original note", CreatedAt: created, UpdatedAt: created,
		},
		branch: {
			ID: "branch", Scope: "global", TopicID: "topic", TopicTitle: "Topic",
			CustomTitle: "branch note", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second),
			Recovered: true, ParentID: "root", RecoveryDepth: 1,
		},
	} {
		if err := agent.SaveBranchMetaPreserveUpdated(path, meta); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.ReconcileDirectory(context.Background(), sessioncatalog.DirectoryTarget{Path: dir, Scope: "global"}); err != nil {
		t.Fatal(err)
	}
	app := &App{tabs: map[string]*WorkspaceTab{}, detachedSessions: map[string]*WorkspaceTab{}}
	app.sessionCatalog.Store(catalog)
	view := app.GetRecoveryLineage(ProjectTopicKey{Scope: "global", TopicID: "topic"})
	if len(view.Members) != 2 {
		t.Fatalf("members = %+v, want original plus recovery version", view.Members)
	}
	byPath := map[string]RecoveryLineageMember{}
	for _, member := range view.Members {
		byPath[member.Path] = member
	}
	if got := byPath[root]; got.VersionNote != "original note" || got.Preview == "" || got.CreatedAt != 10 || got.LastActivityAt == 0 {
		t.Fatalf("original metadata = %+v", got)
	}
	if got := byPath[branch]; got.VersionNote != "branch note" || got.Preview == "" || got.Turns != 2 || got.CreatedAt != 1010 {
		t.Fatalf("branch metadata = %+v", got)
	}
}

func TestGetRecoveryLineageEmptyMembersEncodeAsArray(t *testing.T) {
	view := NewApp().GetRecoveryLineage(ProjectTopicKey{Scope: "global", TopicID: "missing"})
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || string(data) == "null" || view.Members == nil {
		t.Fatalf("empty lineage must keep members as []: %s", data)
	}
}
