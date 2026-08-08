package agent

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type recordingMemoryQueue struct {
	notes []string
}

func (q *recordingMemoryQueue) QueueMemory(note string) {
	q.notes = append(q.notes, note)
}

type memoryQueueProbeTool struct{}

func (memoryQueueProbeTool) Name() string            { return "memory_queue_probe" }
func (memoryQueueProbeTool) Description() string     { return "probe child memory context" }
func (memoryQueueProbeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (memoryQueueProbeTool) ReadOnly() bool          { return true }
func (memoryQueueProbeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if q, ok := memory.QueueFromContext(ctx); ok {
		q.QueueMemory("child injected into parent")
		return "queue present", nil
	}
	return "queue absent", nil
}

func TestSubAgentMasksParentJobsAndMemoryContexts(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(memoryQueueProbeTool{})
	waitTool, ok := tool.LookupBuiltin("wait")
	if !ok {
		t.Fatal("wait builtin not registered")
	}
	reg.Add(waitTool)
	prov := &scriptedProvider{name: "child-context", turns: [][]provider.Chunk{
		{toolCallChunk("probe", "memory_queue_probe", `{}`), {Type: provider.ChunkDone}},
		{{Type: provider.ChunkText, Text: "Child result."}, {Type: provider.ChunkDone}},
	}}
	parentQueue := &recordingMemoryQueue{}
	manager := jobs.NewManager(event.Discard)
	defer manager.Close()
	ctx := memory.WithQueue(jobs.WithManager(context.Background(), manager), parentQueue)
	sess := NewSession("child system")

	answer, err := RunSubAgentWithSession(ctx, prov, reg, sess, "inspect the task", Options{}, event.Discard)
	if err != nil {
		t.Fatalf("RunSubAgentWithSession: %v", err)
	}
	if answer != "Child result." {
		t.Fatalf("answer = %q", answer)
	}
	if len(parentQueue.notes) != 0 {
		t.Fatalf("child injected memory notes into parent queue: %v", parentQueue.notes)
	}
	if got := toolResultByID(sess, "probe"); got != "queue absent" {
		t.Fatalf("memory queue probe result = %q", got)
	}
	for i, req := range prov.requests {
		if !slices.Contains(toolSchemaNames(req.Tools), "wait") {
			t.Fatalf("child request %d lost stable wait schema: %v", i+1, toolSchemaNames(req.Tools))
		}
	}
}
