package sessioncatalog

import "context"

// prepareExactPathProjection avoids publishing unchanged snapshots while
// retaining known counts when only legacy sidecar metadata is stale.
func (c *Catalog) prepareExactPathProjection(ctx context.Context, raw SessionRecord) (SessionRecord, bool) {
	record := classifyRecoveryLineage(normalizeSessionRecord(raw))
	if record.LogicalTopicID == "" {
		record.LogicalTopicID = record.TopicID
	}
	existing, ok, err := c.GetSession(ctx, record.Path)
	if err != nil || !ok || existing.Path == "" || existing.MissingSince != 0 {
		return record, false
	}
	if sameSessionIndexInput(existing, record) {
		return record, true
	}
	if existing.TurnsState != TurnsUnknown && record.TurnsState == TurnsUnknown &&
		existing.ContentFingerprint == record.ContentFingerprint {
		record.Preview = existing.Preview
		record.Turns = existing.Turns
		record.TurnsState = existing.TurnsState
	}
	return record, false
}
