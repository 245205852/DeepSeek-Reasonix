package main

import (
	"slices"
	"testing"
)

// TestRemoteTabCommandsForwardedToServe pins that every command binding
// reaches the right serve endpoint with the mapped body.
func TestRemoteTabCommandsForwardedToServe(t *testing.T) {
	fs := newFakeServe(t, "s3cret", nil)
	kernel := &fakeRemoteKernel{
		statuses:    []RemoteConnectionStatusView{{HostID: "box", State: "connected"}},
		ensureView:  RemoteServerView{HostID: "box", State: "ready", LocalURL: fs.server.URL},
		ensureToken: "s3cret",
	}
	seedBridgeTestHost(t, "box")
	a := &App{remoteRuntime: kernel}
	cleanupRemoteTabPumps(t, a)
	meta := openReadyRemoteTab(t, a, RemoteTabOpenOptions{NewSession: true})

	steps := []struct {
		name string
		call func() error
		want string
	}{
		{"submit", func() error { return a.SubmitRemoteTab(meta.ID, "hello") }, `POST /submit {"input":"hello"}`},
		{"cancel", func() error { return a.CancelRemoteTab(meta.ID) }, "POST /cancel {}"},
		{"approve", func() error { return a.ApproveRemoteTab(meta.ID, "call-1", "allow") }, `POST /approve {"allow":true,"id":"call-1"}`},
		{"approve-deny", func() error { return a.ApproveRemoteTab(meta.ID, "call-2", "deny") }, `POST /approve {"allow":false,"id":"call-2"}`},
		{"answer", func() error {
			return a.AnswerRemoteTab(meta.ID, "ask-1", []RemoteAskAnswer{{QuestionID: "question-1", Selected: []string{"yes"}}})
		}, `POST /answer {"answers":[{"QuestionID":"question-1","Selected":["yes"]}],"id":"ask-1"}`},
		{"rewind", func() error { return a.RewindRemoteTab(meta.ID, "3") }, `POST /rewind {"scope":"both","turn":3}`},
		{"approval-mode", func() error { return a.SetRemoteTabToolApprovalMode(meta.ID, "auto") }, `POST /tool-approval-mode {"mode":"auto"}`},
		{"goal", func() error { return a.SetRemoteTabGoal(meta.ID, "ship it") }, `POST /goal {"goal":"ship it"}`},
		{"effort", func() error { return a.SetRemoteTabEffort(meta.ID, "high") }, `POST /effort {"level":"high"}`},
		{"plan-on", func() error { return a.SetRemoteTabPlanMode(meta.ID, true) }, `POST /plan {"on":true}`},
		{"compact", func() error { return a.CompactRemoteTab(meta.ID) }, "POST /compact {}"},
		{"fork", func() error { return a.ForkRemoteTab(meta.ID, 2, "try-auth") }, `POST /fork {"name":"try-auth","turn":2}`},
		{"summarize", func() error { return a.SummarizeRemoteTab(meta.ID, 4, "upto") }, `POST /summarize {"mode":"upto","turn":4}`},
		{"forget", func() error { return a.ForgetRemoteTab(meta.ID, "api-key") }, `POST /forget {"name":"api-key"}`},
	}
	for _, step := range steps {
		if err := step.call(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	calls := fs.recorded()
	for _, step := range steps {
		if !slices.Contains(calls, step.want) {
			t.Fatalf("%s: serve saw %v, want %q", step.name, calls, step.want)
		}
	}
	if _, err := a.RemoteTabBranches(meta.ID); err != nil {
		t.Fatalf("branches: %v", err)
	}
	if _, err := a.RemoteTabSkills(meta.ID); err != nil {
		t.Fatalf("skills: %v", err)
	}
	foundBranches, foundSkills := false, false
	for _, c := range fs.recorded() {
		if c == "GET /branches " {
			foundBranches = true
		}
		if c == "GET /skills " {
			foundSkills = true
		}
	}
	if !foundBranches || !foundSkills {
		t.Fatalf("branches/skills reads missing: %v", fs.recorded())
	}
}
