package agent

import "reasonix/internal/tool"

// readOnlyAgentConstruction is the single pairing every strictly read-only
// loop shares: the permanent ReadOnlyExecution flag plus the final registry
// filter. Batch children and legacy read-only call sites use this boundary.
func readOnlyAgentConstruction(reg *tool.Registry, opts Options) (*tool.Registry, Options) {
	opts.ReadOnlyExecution = true
	opts.PlannerMCPExecution = false
	return strictReadOnlyExecutionRegistry(reg), opts
}
