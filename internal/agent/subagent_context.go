package agent

import (
	"context"

	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/tool"
)

func subagentProviderContext(ctx context.Context) context.Context {
	ctx = tool.WithoutGoalTurnRecorder(ctx)
	ctx = jobs.WithoutManager(ctx)
	return memory.WithoutQueue(ctx)
}
