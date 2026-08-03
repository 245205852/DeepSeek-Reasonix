package cli

import (
	"time"

	"reasonix/internal/agent"
)

// reclaimCLIRecoveryBranches performs the same conservative recovery-copy
// hygiene as Desktop. Failures are intentionally silent: cleanup is optional,
// and any lease, concurrent save, I/O error, or failed revalidation must leave
// the recoverable conversation in place for a later run.
func reclaimCLIRecoveryBranches(dir string) {
	candidates, err := agent.ReclaimableRecoveryBranches(dir, time.Now(), agent.RecoveryGCGracePeriod)
	if err != nil {
		return
	}
	for _, path := range candidates {
		_ = agent.TrashReclaimableRecoveryBranch(path, dir)
	}
}
