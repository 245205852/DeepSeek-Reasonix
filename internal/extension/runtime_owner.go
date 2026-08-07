package extension

import (
	"context"
	"fmt"
	"sync/atomic"
)

// RuntimeOwner owns generation-scoped lifecycle state for one logical
// controller/session lineage. Rebuilds reuse the owner; independent sessions
// receive independent owners so publishing one runtime never drains another.
type RuntimeOwner struct {
	Gate        *PublishGate
	Receipts    *ReceiptStore
	FilePriors  *FilePriorStore
	Messages    *MessageSendGuard
	HostStreams *HostStreamRegistry
	receiptSeq  atomic.Uint64
}

// NewRuntimeOwner returns an isolated runtime lifecycle owner.
func NewRuntimeOwner() *RuntimeOwner {
	receipts := NewReceiptStore()
	gate := newPublishGate(receipts)
	owner := &RuntimeOwner{
		Gate:       gate,
		Receipts:   receipts,
		FilePriors: NewFilePriorStore(),
		Messages:   NewMessageSendGuard(),
	}
	owner.HostStreams = NewHostStreamRegistry(gate)
	return owner
}

// DefaultRuntimeOwner preserves package-level compatibility for callers that
// have not yet supplied an explicit owner. Product boot paths use isolated
// owners instead.
var DefaultRuntimeOwner = NewRuntimeOwner()

// ContextWithRuntimeOwner binds owner to provider/agent work derived from ctx.
func ContextWithRuntimeOwner(ctx context.Context, owner *RuntimeOwner) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if owner == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeOwnerContextKey{}, owner)
}

// RuntimeOwnerFromContext returns the bound owner, falling back to the package
// compatibility owner for callers outside the product boot path.
func RuntimeOwnerFromContext(ctx context.Context) *RuntimeOwner {
	if ctx != nil {
		if owner, ok := ctx.Value(runtimeOwnerContextKey{}).(*RuntimeOwner); ok && owner != nil {
			return owner
		}
	}
	return DefaultRuntimeOwner
}

type runtimeOwnerContextKey struct{}

// RecordProviderSubmit records one irreversible provider request.
func (o *RuntimeOwner) RecordProviderSubmit(generation uint64, streamID, owner string) {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	o.Receipts.Record(EffectReceipt{
		ID:                 "provider-submit:" + streamID,
		Owner:              owner,
		Generation:         generation,
		Class:              Irreversible,
		CompensationStatus: "not_applicable",
	})
}

// RecordMessageSentOnce records a user-visible send exactly once per
// generation/message pair in this runtime lineage.
func (o *RuntimeOwner) RecordMessageSentOnce(generation uint64, messageID, owner string) bool {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	if !o.Messages.TryRecord(generation, messageID) {
		return false
	}
	o.Receipts.Record(EffectReceipt{
		ID:                 "message-sent:" + messageID,
		Owner:              owner,
		Generation:         generation,
		Class:              Irreversible,
		CompensationStatus: "not_applicable",
	})
	return true
}

// RecordMessageSent records a user-visible send without applying deduplication.
func (o *RuntimeOwner) RecordMessageSent(generation uint64, messageID, owner string) {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	o.Receipts.Record(EffectReceipt{
		ID:                 "message-sent:" + messageID,
		Owner:              owner,
		Generation:         generation,
		Class:              Irreversible,
		CompensationStatus: "not_applicable",
	})
}

// RecordFileWrite captures prior state under a unique receipt ID. Repeated
// writes to the same path never overwrite an earlier generation's evidence.
func (o *RuntimeOwner) RecordFileWrite(path string, hadPrior bool, prior []byte) string {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	gen := o.Gate.Published()
	id := fmt.Sprintf("file-write:%d:%d", gen, o.receiptSeq.Add(1))
	o.FilePriors.Capture(id, path, prior, hadPrior)
	o.Receipts.Record(EffectReceipt{
		ID:                 id,
		Owner:              "write_file",
		Generation:         gen,
		Class:              Compensatable,
		CompensationStatus: "prior_captured",
		Error:              fmt.Sprintf("prior_bytes=%d", len(prior)),
	})
	return id
}

// ApplyFileWriteCompensation restores prior file state and updates this
// lineage's receipt without touching another runtime owner.
func (o *RuntimeOwner) ApplyFileWriteCompensation(receiptID string) error {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	if err := o.FilePriors.Compensate(receiptID); err != nil {
		o.Receipts.Record(EffectReceipt{
			ID:                 receiptID,
			Class:              Compensatable,
			CompensationStatus: "failed",
			Error:              err.Error(),
		})
		return err
	}
	o.FilePriors.Forget(receiptID)
	o.Receipts.Record(EffectReceipt{
		ID:                 receiptID,
		Class:              Compensatable,
		CompensationStatus: "applied",
	})
	return nil
}

// DecideResume evaluates recovery evidence owned by this runtime lineage.
func (o *RuntimeOwner) DecideResume(generation uint64) ResumeDecision {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	return DecideResume(o.Receipts, generation)
}

// AssessRecoverability evaluates recovery evidence owned by this lineage.
func (o *RuntimeOwner) AssessRecoverability(generation uint64) Recoverability {
	if o == nil {
		o = DefaultRuntimeOwner
	}
	return o.Receipts.AssessRecoverability(generation)
}
