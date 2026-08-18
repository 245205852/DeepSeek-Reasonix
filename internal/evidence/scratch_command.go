package evidence

import (
	"path/filepath"
	"strings"

	"reasonix/internal/shellsafe"
)

// BashScratchDeliveryScope recognizes a narrow interpreter-plus-script shape.
// It only controls delivery accounting; checkpoint coverage must stay fail-closed.
func BashScratchDeliveryScope(command, workspaceRoot string, scratchRoots []string) bool {
	argv, envPrefixed, ok := shellsafe.CommandArgv(command)
	if !ok || envPrefixed || len(argv) != 2 {
		return false
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(argv[0]), ".exe"))
	switch base {
	case "python", "python3", "node", "ruby", "perl", "bash", "sh":
	default:
		return false
	}
	script := strings.TrimSpace(argv[1])
	return script != "" && !strings.HasPrefix(script, "-") &&
		ClassifyWriteScope(script, workspaceRoot, scratchRoots) == WriteScopeScratch
}
