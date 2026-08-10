package agent

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/shellparse"
)

// workspaceMutationForCall classifies host resource invalidation independently
// from the delivery evidence ledger. Delivery asks whether a call invalidates a
// completed-review receipt; the desktop asks which workspace resources may have
// changed. Those contracts intentionally differ for operations such as a bare
// git commit, which changes HEAD/index/history without changing file contents.
func workspaceMutationForCall(toolID, toolName string, args json.RawMessage, readOnly bool) (event.WorkspaceMutation, bool) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" || readOnly || evidence.IsNonMutationMetaTool(toolName) || workspaceHostStateOnlyTool(toolName) {
		return event.WorkspaceMutation{}, false
	}

	paths := evidence.ToolCallPaths(args)
	mutation := event.WorkspaceMutation{
		ToolID:      toolID,
		ToolName:    toolName,
		Paths:       paths,
		AllPaths:    toolName == "bash" || len(paths) == 0,
		Content:     true,
		Tree:        true,
		WorkingTree: true,
	}
	if toolName == "bash" {
		mutation.GitMeta = bashMayChangeGitMetadata(bashCommandFromArgs(args))
	}
	return mutation, true
}

func workspaceHostStateOnlyTool(toolName string) bool {
	if strings.HasPrefix(toolName, "mcp_connect__") {
		return true
	}
	switch toolName {
	case "kill_shell", "remember", "forget":
		return true
	default:
		return false
	}
}

func bashMayChangeGitMetadata(command string) bool {
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return commandMentionsGitExecutable(command)
	}
	for _, segment := range segments {
		fields, malformed := shellparse.StaticFields(strings.TrimSpace(segment))
		if malformed != "" {
			if commandMentionsGitExecutable(segment) {
				return true
			}
			continue
		}
		fields = stripWorkspaceEnvPrefix(fields)
		if len(fields) == 0 || !isGitExecutable(fields[0]) {
			continue
		}
		// A non-read-only bash call that reaches an explicit git executable is
		// conservatively Git metadata changing. Host read-only classification
		// has already removed status/diff/log-style calls before this point.
		return true
	}
	return false
}

func stripWorkspaceEnvPrefix(fields []string) []string {
	for len(fields) > 0 {
		if fields[0] == "env" || (strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-")) {
			fields = fields[1:]
			continue
		}
		break
	}
	return fields
}

func commandMentionsGitExecutable(command string) bool {
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, "'\"(){}[];|&")
		if isGitExecutable(field) {
			return true
		}
	}
	return false
}

func isGitExecutable(field string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(field)))
	return base == "git" || base == "git.exe"
}
