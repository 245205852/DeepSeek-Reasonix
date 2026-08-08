package agent

import (
	"strings"

	"reasonix/internal/tool"
)

var plannerNonResearchTools = []string{
	"ask",
	"bash_output",
	"complete_step",
	"slash_command",
	"todo_write",
	"update_goal",
	"wait",
}

// PlannerToolRegistry returns read-only research tools plus an isolated
// use_capability proxy. Workflow and direct MCP schemas stay hidden.
func PlannerToolRegistry(parent *tool.Registry) *tool.Registry {
	exclude := append(SubagentMetaTools(), plannerNonResearchTools...)
	base := FilterReadOnlyRegistry(parent, exclude...)
	sub := tool.NewRegistry()
	if base != nil {
		for _, name := range base.Names() {
			if name == "use_capability" || strings.HasPrefix(name, tool.MCPNamePrefix) {
				continue
			}
			if tl, ok := base.Get(name); ok {
				if classifier, ok := tl.(tool.PlanModeClassifier); ok && !classifier.PlanModeSafe() {
					continue
				}
				sub.Add(tl)
			}
		}
	}
	if parent != nil {
		if tl, ok := parent.Get("use_capability"); ok {
			if uc, ok := tl.(*UseCapabilityTool); ok {
				sub.Add(uc.CloneForAgent(nil, nil))
			} else {
				sub.Add(tl)
			}
		}
	}
	return sub
}
