package sidecar

import (
	"fmt"
	"strings"

	"reasonix/internal/extension/protocol"
	"reasonix/internal/pluginpkg"
)

// validateProvidesCeiling rejects handshake Provides entries that are not a
// subset of the manifest capability ceiling (by namespace/kind/id).
func validateProvidesCeiling(manifest []pluginpkg.CapabilityRef, wire []protocol.CapabilityWire) error {
	if len(wire) == 0 {
		return nil
	}
	if len(manifest) == 0 {
		// No v2 provides list: fall back to runtime.capabilities string checks above.
		return nil
	}
	allowed := map[string]bool{}
	for _, p := range manifest {
		key := strings.TrimSpace(p.Namespace) + "/" + strings.TrimSpace(p.Kind) + "/" + strings.TrimSpace(p.ID)
		allowed[key] = true
	}
	for _, w := range wire {
		key := strings.TrimSpace(w.Namespace) + "/" + strings.TrimSpace(w.Kind) + "/" + strings.TrimSpace(w.ID)
		if !allowed[key] {
			return fmt.Errorf("handshake provides %q is outside the manifest provides ceiling", key)
		}
	}
	return nil
}
