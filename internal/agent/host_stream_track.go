package agent

import (
	"context"

	"reasonix/internal/extension"
)

// trackPublishedHostStream binds cancel to the published generation so drain
// timeout aborts host provider HTTP streams. No-op when gen is 0.
func trackPublishedHostStream(cancel context.CancelFunc) (untrack func()) {
	gen := extension.DefaultPublishGate().Published()
	if gen == 0 || cancel == nil {
		return func() {}
	}
	return extension.TrackHostStream(gen, cancel)
}
