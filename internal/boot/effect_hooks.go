package boot

import (
	"fmt"

	"reasonix/internal/extension"
	"reasonix/internal/tool/builtin"
)

func init() {
	// Wire tool-level effect receipts without creating a tool→extension import
	// edge (extension tests import builtin adapters).
	builtin.FileWriteReceipt = func(path string, hadPrior bool, prior []byte) {
		gen := extension.DefaultPublishGate().Published()
		id := "file-write:" + path
		if hadPrior {
			extension.DefaultFilePriorStore.Capture(id, path, prior, true)
			extension.DefaultReceiptStore.Record(extension.EffectReceipt{
				ID:                 id,
				Owner:              "write_file",
				Generation:         gen,
				Class:              extension.Compensatable,
				CompensationStatus: "prior_captured",
				Error:              fmt.Sprintf("prior_bytes=%d", len(prior)),
			})
			return
		}
		createID := "file-create:" + path
		extension.DefaultFilePriorStore.Capture(createID, path, nil, false)
		extension.DefaultReceiptStore.Record(extension.EffectReceipt{
			ID:                 createID,
			Owner:              "write_file",
			Generation:         gen,
			Class:              extension.Compensatable,
			CompensationStatus: "prior_captured",
		})
	}
}
