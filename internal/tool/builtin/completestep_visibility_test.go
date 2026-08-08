package builtin

import (
	"context"
	"testing"

	"reasonix/internal/planmode"
	"reasonix/internal/tool"
)

func TestCompleteStepSchemaOnlyVisibleAfterPlanApproval(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(completeStep{})
	if got := reg.SchemasForContext(planmode.WithActive(context.Background(), true)); len(got) != 0 {
		t.Fatalf("Plan-mode schemas = %+v, want complete_step hidden", got)
	}
	got := reg.SchemasForContext(planmode.WithActive(context.Background(), false))
	if len(got) != 1 || got[0].Name != "complete_step" {
		t.Fatalf("execution schemas = %+v, want complete_step", got)
	}
}
