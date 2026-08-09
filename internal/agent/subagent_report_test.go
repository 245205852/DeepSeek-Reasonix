package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func intPtr(v int) *int { return &v }

func TestHostReceiptsAttestChangesAndVerifications(t *testing.T) {
	summary := evidence.ChildEvidenceSummary{Receipts: []evidence.Receipt{
		{ToolName: "write_file", Success: true, Mutation: true, Paths: []string{"parser.go"}},
		{ToolName: "write_file", Success: true, Mutation: true, Paths: []string{"parser_test.go"}},
		{ToolName: "bash", Success: true, Command: "go test ./parser", ExitCode: intPtr(0), Verification: evidence.VerificationPassed},
		{ToolName: "bash", Success: true, Command: "ls -la", ExitCode: intPtr(0), Verification: evidence.VerificationNotVerification},
	}}

	got := formatHostReceipts(summary, WritePathSet{})
	for _, want := range []string{"changed: parser.go, parser_test.go", "go test ./parser (verification passed, exit 0)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("receipts block %q missing %q", got, want)
		}
	}
	// A plain read command is not a claim the parent must adjudicate, so it
	// never spends parent context.
	if strings.Contains(got, "ls -la") {
		t.Fatalf("receipts block must not list non-verification commands: %q", got)
	}
}

func TestHostReceiptsStaySilentForReadOnlyChildren(t *testing.T) {
	summary := evidence.ChildEvidenceSummary{Receipts: []evidence.Receipt{
		{ToolName: "read_file", Success: true, Read: true, Paths: []string{"parser.go"}},
		{ToolName: "grep", Success: true, Read: true},
	}}
	if got := formatHostReceipts(summary, WritePathSet{}); got != "" {
		t.Fatalf("read-only child produced a receipts block: %q", got)
	}
	if got := appendHostReceipts("just prose", summary, WritePathSet{}); got != "just prose" {
		t.Fatalf("answer = %q, want it unchanged", got)
	}
}

func TestHostReceiptsRecordFailedCommands(t *testing.T) {
	summary := evidence.ChildEvidenceSummary{Receipts: []evidence.Receipt{
		{ToolName: "bash", Success: true, Command: "go build ./...", ExitCode: intPtr(2)},
		{ToolName: "bash", Success: true, Command: "go test ./parser", ExitCode: intPtr(1), Verification: evidence.VerificationFailed},
	}}
	got := formatHostReceipts(summary, WritePathSet{})
	for _, want := range []string{"go build ./... (exit 2)", "go test ./parser (verification failed, exit 1)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("receipts block %q missing %q", got, want)
		}
	}
}

func TestDecorateExecutionReceiptCarriesHostObservedOutcome(t *testing.T) {
	rec := evidence.Receipt{ToolName: "bash", Success: true}
	decorateExecutionReceipt(&rec, "  out  ", &tool.ShellExecution{
		ExitCode:     tool.IntPtr(3),
		Verification: tool.ShellVerificationFailed,
	})
	if rec.ExitCode == nil || *rec.ExitCode != 3 {
		t.Fatalf("ExitCode = %v, want 3", rec.ExitCode)
	}
	if rec.Verification != evidence.VerificationFailed {
		t.Fatalf("Verification = %q, want %q", rec.Verification, evidence.VerificationFailed)
	}
	if rec.OutputBytes != len("out") {
		t.Fatalf("OutputBytes = %d, want %d", rec.OutputBytes, len("out"))
	}
	// A tool that ran no process must not gain a fabricated exit status.
	plain := evidence.Receipt{ToolName: "read_file", Success: true}
	decorateExecutionReceipt(&plain, "body", nil)
	if plain.ExitCode != nil {
		t.Fatalf("non-shell receipt gained ExitCode %v", plain.ExitCode)
	}
}

type fakeWriteFileTool struct{}

func (fakeWriteFileTool) Name() string        { return "write_file" }
func (fakeWriteFileTool) Description() string { return "Write a file." }
func (fakeWriteFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
}
func (fakeWriteFileTool) ReadOnly() bool { return false }
func (fakeWriteFileTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "written", nil
}

// The parent must learn what the child actually changed even when the child's
// own prose says nothing about it.
func TestSubAgentAnswerCarriesHostReceipts(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(fakeWriteFileTool{})
	prov := &scriptedProvider{name: "p", turns: [][]provider.Chunk{
		{toolCallChunk("1", "write_file", `{"path":"parser.go"}`), {Type: provider.ChunkDone}},
		{{Type: provider.ChunkText, Text: "all done"}, {Type: provider.ChunkDone}},
	}}

	answer, err := RunSubAgentWithSession(context.Background(), prov, reg, NewSession("sys"),
		"fix the parser", Options{}, event.Discard)
	if err != nil {
		t.Fatalf("RunSubAgentWithSession: %v", err)
	}
	if !strings.Contains(answer, "all done") {
		t.Fatalf("answer lost the child's own summary: %q", answer)
	}
	if !strings.Contains(answer, hostReceiptsHeader) || !strings.Contains(answer, "parser.go") {
		t.Fatalf("answer missing host receipts for the write it performed: %q", answer)
	}
}
