package builtin

import (
	"testing"

	"reasonix/internal/tool"
)

func TestCompleteStepSchemaStableAcrossPlanModes(t *testing.T) {
	reg := tool.NewRegistry()
	reg.Add(completeStep{})
	got := reg.Schemas()
	if len(got) != 1 || got[0].Name != "complete_step" {
		t.Fatalf("provider schemas = %+v, want stable complete_step schema", got)
	}
}
