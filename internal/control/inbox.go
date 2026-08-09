package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"reasonix/internal/event"
	"reasonix/internal/sessioninbox"
)

// TurnAdmission is the exported classification of TrySubmitInboxItem /
// TrySteerInboxItem results.
type TurnAdmission string

const (
	AdmissionStarted          TurnAdmission = "started"
	AdmissionSteerAccepted    TurnAdmission = "steer_accepted"
	AdmissionQueuedFollowup   TurnAdmission = "queued_followup"
	AdmissionRejectedBusy     TurnAdmission = "rejected_busy"
	AdmissionRejectedRotating TurnAdmission = "rejected_rotating"
	AdmissionRejectedClosed   TurnAdmission = "rejected_closed"
	AdmissionRejectedCapacity TurnAdmission = "rejected_capacity"
)

// InboxRequest is the frontend-facing enqueue payload.
type InboxRequest struct {
	Intent      sessioninbox.InboxIntent
	Display     string
	Raw         string
	Submit      string
	Format      string
	Source      string
	Idempotency string
	// FreezeRefs lists workspace-relative paths to freeze at enqueue time.
	FreezeRefs []string
}

// Inbox port on SessionAPI.
type Inbox interface {
	EnqueueInbox(req InboxRequest) (sessioninbox.InboxReceipt, error)
	InboxSnapshot() sessioninbox.InboxSnapshot
	ReadInboxItem(id string) (sessioninbox.InboxItemMeta, sessioninbox.PromptEnvelope, error)
	UpdateInboxItem(id string, display, raw, submit string) (sessioninbox.InboxItemMeta, error)
	DeleteInboxItem(id string) error
	CancelWithInboxItems(ids []string) error
	MoveInboxItem(id string, toIndex int) error
	SetInboxPaused(paused bool) error
	RetryInboxItem(id string) error
	RefreshInboxReferences(id string) error
	TrySubmitInboxItem(id string) (sessioninbox.InboxReceipt, error)
	TrySteerInboxItem(id string) (sessioninbox.InboxReceipt, error)
	TryEnqueueAndSteer(req InboxRequest) (sessioninbox.InboxReceipt, error)
	TryEnqueueFollowup(req InboxRequest) (sessioninbox.InboxReceipt, error)
}

// Compile-time port satisfaction.
var _ Inbox = (*Controller)(nil)

// inboxState is controller-owned inbox wiring (disk store + active items).
type inboxState struct {
	mu    sync.Mutex
	store *sessioninbox.Store
	// activeItemIDs tracks every item admitted during the current turn:
	// the running follow-up (if any) plus every accepted steer. TurnDone
	// durable-acks the whole set so multi-steer rounds never leave
	// steer_consumed orphans.
	activeItemIDs map[string]struct{}
	dispatching   bool
}

func (s *inboxState) trackActive(id string) {
	if s == nil || id == "" {
		return
	}
	if s.activeItemIDs == nil {
		s.activeItemIDs = make(map[string]struct{})
	}
	s.activeItemIDs[id] = struct{}{}
}

func (s *inboxState) untrackActive(id string) {
	if s == nil || s.activeItemIDs == nil || id == "" {
		return
	}
	delete(s.activeItemIDs, id)
}

func (s *inboxState) takeActive() []string {
	if s == nil || len(s.activeItemIDs) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.activeItemIDs))
	for id := range s.activeItemIDs {
		out = append(out, id)
	}
	s.activeItemIDs = nil
	return out
}

func (s *inboxState) clearActive() {
	if s == nil {
		return
	}
	s.activeItemIDs = nil
}

func (c *Controller) ensureInbox() (*sessioninbox.Store, error) {
	path := c.SessionPath()
	if path == "" {
		return nil, fmt.Errorf("inbox requires a persisted session path")
	}
	c.inbox.mu.Lock()
	defer c.inbox.mu.Unlock()
	if c.inbox.store != nil && c.inbox.store.SessionPath() == path {
		return c.inbox.store, nil
	}
	if c.inbox.store != nil {
		c.inbox.store.Close()
		c.inbox.store = nil
	}
	st, err := sessioninbox.Open(path, sessioninbox.Limits{})
	if err != nil {
		return nil, err
	}
	c.inbox.store = st
	snap := st.Snapshot()
	if snap.Recovered && snap.RecoveredN > 0 {
		c.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Code:  "inbox_recovered",
			Text:  fmt.Sprintf("Recovered %d pending instruction(s). Inbox is paused — review with /queue before resuming.", snap.RecoveredN),
		})
		sessioninbox.NoteRecovered(snap.RecoveredN)
	}
	return st, nil
}

// rebindInbox opens the inbox for the current session path. Safe across
// NewSession/Resume/SetSessionPath; does not copy items on fork.
func (c *Controller) rebindInbox() {
	path := c.SessionPath()
	c.inbox.mu.Lock()
	defer c.inbox.mu.Unlock()
	if c.inbox.store != nil {
		if path != "" && c.inbox.store.SessionPath() == path {
			return
		}
		// Pause the old session's queue so it is not auto-run if reopened.
		_ = c.inbox.store.SetPaused(true)
		c.inbox.store.Close()
		c.inbox.store = nil
		c.inbox.clearActive()
	}
	if path == "" {
		return
	}
	st, err := sessioninbox.Open(path, sessioninbox.Limits{})
	if err != nil {
		slog.Warn("controller: open session inbox", "err", err, "path", path)
		return
	}
	c.inbox.store = st
	snap := st.Snapshot()
	if snap.Recovered && snap.RecoveredN > 0 {
		// Emit after unlock via deferred sink call would race; emit here.
		go func(n int) {
			c.sink.Emit(event.Event{
				Kind:  event.Notice,
				Level: event.LevelWarn,
				Code:  "inbox_recovered",
				Text:  fmt.Sprintf("Recovered %d pending instruction(s). Inbox is paused — review with /queue before resuming.", n),
			})
		}(snap.RecoveredN)
		sessioninbox.NoteRecovered(snap.RecoveredN)
	}
}

func (c *Controller) pauseInboxOnRotate() {
	c.inbox.mu.Lock()
	st := c.inbox.store
	c.inbox.mu.Unlock()
	if st != nil {
		_ = st.SetPaused(true)
	}
}

// EnqueueInbox durably queues an instruction. Only returns a receipt after
// blob+manifest commit. Does not auto-start a turn (call TrySubmit / dispatcher).
func (c *Controller) EnqueueInbox(req InboxRequest) (sessioninbox.InboxReceipt, error) {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxReceipt{}, err
	}
	submit := strings.TrimSpace(firstNonEmptyStr(req.Submit, req.Raw, req.Display))
	if submit == "" {
		return sessioninbox.InboxReceipt{}, sessioninbox.ErrEmpty
	}
	display := firstNonEmptyStr(req.Display, submit)
	raw := firstNonEmptyStr(req.Raw, submit)
	env := sessioninbox.PromptEnvelope{
		DisplayText: display,
		RawText:     raw,
		SubmitText:  submit,
		Format:      req.Format,
		Source:      req.Source,
		Idempotency: req.Idempotency,
	}
	if len(req.FreezeRefs) > 0 {
		refs, ferr := sessioninbox.FreezeRefs(context.Background(), c.WorkspaceRoot(), req.FreezeRefs)
		if ferr != nil {
			slog.Warn("controller: freeze inbox refs", "err", ferr)
		}
		env.Refs = refs
	} else if c.HasRefs(submit) {
		// Best-effort: extract path tokens from @-refs already present.
		// Full resolution still happens at consumption time for clean git.
		if tokens := extractRefPathTokens(submit); len(tokens) > 0 {
			refs, _ := sessioninbox.FreezeRefs(context.Background(), c.WorkspaceRoot(), tokens)
			env.Refs = refs
		}
	}
	intent := req.Intent
	if intent != sessioninbox.IntentSteer {
		intent = sessioninbox.IntentFollowup
	}
	rec, err := st.Enqueue(sessioninbox.EnqueueRequest{
		Intent:      intent,
		Envelope:    env,
		Source:      req.Source,
		Idempotency: req.Idempotency,
		SessionID:   c.parentSessionID(),
	})
	if err != nil {
		if errors.Is(err, sessioninbox.ErrCapacityItems) || errors.Is(err, sessioninbox.ErrCapacityBytes) || errors.Is(err, sessioninbox.ErrItemTooLarge) {
			sessioninbox.NoteCapacityReject()
		} else {
			sessioninbox.NoteTxFail()
		}
		return sessioninbox.InboxReceipt{}, err
	}
	sessioninbox.NoteEnqueue(int64(len(env.SubmitText)))
	return rec, nil
}

func (c *Controller) InboxSnapshot() sessioninbox.InboxSnapshot {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxSnapshot{}
	}
	return st.Snapshot()
}

func (c *Controller) ReadInboxItem(id string) (sessioninbox.InboxItemMeta, sessioninbox.PromptEnvelope, error) {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxItemMeta{}, sessioninbox.PromptEnvelope{}, err
	}
	return st.ReadItem(id)
}

func (c *Controller) UpdateInboxItem(id, display, raw, submit string) (sessioninbox.InboxItemMeta, error) {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxItemMeta{}, err
	}
	submit = strings.TrimSpace(firstNonEmptyStr(submit, raw, display))
	display = firstNonEmptyStr(display, submit)
	raw = firstNonEmptyStr(raw, submit)
	env := sessioninbox.PromptEnvelope{
		DisplayText: display,
		RawText:     raw,
		SubmitText:  submit,
	}
	if c.HasRefs(submit) {
		if tokens := extractRefPathTokens(submit); len(tokens) > 0 {
			refs, _ := sessioninbox.FreezeRefs(context.Background(), c.WorkspaceRoot(), tokens)
			env.Refs = refs
		}
	}
	return st.UpdateItem(id, env)
}

func (c *Controller) DeleteInboxItem(id string) error {
	st, err := c.ensureInbox()
	if err != nil {
		return err
	}
	c.inbox.mu.Lock()
	c.inbox.untrackActive(id)
	c.inbox.mu.Unlock()
	return st.DeleteItem(id)
}

// CancelWithInboxItems stops the active turn and discards only the durable
// pending items explicitly owned by the cancelling frontend. Admission is
// paused around the batch deletion so TurnDone cannot race a cancelled item
// into a new provider turn. Unrelated inbox items remain intact.
func (c *Controller) CancelWithInboxItems(ids []string) error {
	st, err := c.ensureInbox()
	if err != nil {
		c.Cancel()
		return err
	}
	if err := st.SetPaused(true); err != nil {
		c.Cancel()
		return err
	}
	if err := st.DiscardPendingItems(ids); err != nil {
		// Keep the inbox paused for inspection if an item already crossed the
		// admission boundary. Cancellation still stops that in-flight turn.
		c.Cancel()
		return err
	}
	c.inbox.mu.Lock()
	for _, id := range ids {
		c.inbox.untrackActive(strings.TrimSpace(id))
	}
	c.inbox.mu.Unlock()
	c.Cancel()
	if err := st.SetPaused(false); err != nil {
		return err
	}
	return nil
}

func (c *Controller) MoveInboxItem(id string, toIndex int) error {
	st, err := c.ensureInbox()
	if err != nil {
		return err
	}
	return st.MoveItem(id, toIndex)
}

func (c *Controller) SetInboxPaused(paused bool) error {
	st, err := c.ensureInbox()
	if err != nil {
		return err
	}
	if err := st.SetPaused(paused); err != nil {
		return err
	}
	if paused {
		sessioninbox.NotePaused()
	} else {
		// On resume, try to dispatch if idle.
		c.maybeDispatchInbox()
	}
	return nil
}

func (c *Controller) RetryInboxItem(id string) error {
	st, err := c.ensureInbox()
	if err != nil {
		return err
	}
	if err := st.RetryItem(id); err != nil {
		return err
	}
	c.maybeDispatchInbox()
	return nil
}

func (c *Controller) RefreshInboxReferences(id string) error {
	st, err := c.ensureInbox()
	if err != nil {
		return err
	}
	meta, env, err := st.ReadItem(id)
	if err != nil {
		return err
	}
	_ = meta
	paths := make([]string, 0, len(env.Refs))
	for _, r := range env.Refs {
		if p := firstNonEmptyStr(r.Path, r.DisplayPath); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 && c.HasRefs(env.SubmitText) {
		paths = extractRefPathTokens(env.SubmitText)
	}
	refs, _ := sessioninbox.FreezeRefs(context.Background(), c.WorkspaceRoot(), paths)
	env.Refs = refs
	_, err = st.UpdateItem(id, env)
	return err
}

// TrySteerInboxItem persists intent=steer (if needed) and attempts mid-turn
// admission. Rejected steers stay queued as follow-up.
//
// The agent loader only captures the item ID and re-reads the blob on consume
// so large steer bodies do not accumulate in the agent heap.
func (c *Controller) TrySteerInboxItem(id string) (sessioninbox.InboxReceipt, error) {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxReceipt{}, err
	}
	meta, _, err := st.ReadItem(id)
	if err != nil {
		return sessioninbox.InboxReceipt{}, err
	}
	if meta.State != sessioninbox.StateQueued && meta.State != sessioninbox.StateSteerAccepted {
		return sessioninbox.InboxReceipt{}, sessioninbox.ErrInvalidState
	}
	c.mu.Lock()
	exec := c.executor
	running := c.running
	rotating := c.rotating
	closed := c.closed
	c.mu.Unlock()
	cap := st.Snapshot().Capacity
	if closed {
		return sessioninbox.InboxReceipt{ItemID: id, Disposition: sessioninbox.DispositionRejectedClosed, Capacity: cap}, nil
	}
	if rotating {
		return sessioninbox.InboxReceipt{ItemID: id, Disposition: sessioninbox.DispositionRejectedRotating, Capacity: cap}, nil
	}
	// Capture only the store pointer + item id. Load body from disk at consume.
	storeRef := st
	itemID := id
	loader := func() (string, error) {
		_, env, err := storeRef.ReadItem(itemID)
		if err != nil {
			return "", err
		}
		text := strings.TrimSpace(env.SubmitText)
		if text == "" {
			text = strings.TrimSpace(env.DisplayText)
		}
		if text == "" {
			return "", fmt.Errorf("inbox item %s has empty body", itemID)
		}
		// Apply frozen refs at consume so the model sees enqueue-time content.
		if len(env.Refs) > 0 {
			block, bodies, merr := sessioninbox.MaterializeRefs(context.Background(), c.WorkspaceRoot(), env.Refs)
			if merr != nil {
				return "", merr
			}
			if block != "" {
				return "", fmt.Errorf("frozen reference unavailable: %s", block)
			}
			text = sessioninbox.ApplyFrozenRefs(text, bodies)
		}
		return text, nil
	}
	accepted := running && exec != nil && exec.SteerItem(id, loader)
	if accepted {
		_ = st.SetState(id, sessioninbox.StateSteerAccepted, "")
		c.inbox.mu.Lock()
		c.inbox.trackActive(id)
		c.inbox.mu.Unlock()
		sessioninbox.NoteSteerAccepted()
		return sessioninbox.InboxReceipt{
			ItemID:      id,
			Disposition: sessioninbox.DispositionSteerAccepted,
			Paused:      st.Snapshot().Paused,
			Capacity:    cap,
		}, nil
	}
	// Rejected: keep as follow-up.
	_ = st.ConvertIntent(id, sessioninbox.IntentFollowup)
	_ = st.SetState(id, sessioninbox.StateQueued, "")
	sessioninbox.NoteSteerRejected()
	return sessioninbox.InboxReceipt{
		ItemID:      id,
		Disposition: sessioninbox.DispositionQueuedFollowup,
		Paused:      st.Snapshot().Paused,
		Capacity:    cap,
	}, nil
}

// TrySubmitInboxItem admits a queued item as a new turn when the session is idle.
func (c *Controller) TrySubmitInboxItem(id string) (sessioninbox.InboxReceipt, error) {
	st, err := c.ensureInbox()
	if err != nil {
		return sessioninbox.InboxReceipt{}, err
	}
	meta, env, err := st.ReadItem(id)
	if err != nil {
		return sessioninbox.InboxReceipt{}, err
	}
	if meta.State != sessioninbox.StateQueued && meta.State != sessioninbox.StateUncertain {
		return sessioninbox.InboxReceipt{}, sessioninbox.ErrInvalidState
	}
	// Blocked items must be refreshed first.
	if meta.State == sessioninbox.StateBlocked {
		return sessioninbox.InboxReceipt{}, sessioninbox.ErrInvalidState
	}
	// Materialize frozen refs and inject them into the model-visible submit text.
	submit := env.SubmitText
	if len(env.Refs) > 0 {
		block, bodies, merr := sessioninbox.MaterializeRefs(context.Background(), c.WorkspaceRoot(), env.Refs)
		if merr != nil {
			return sessioninbox.InboxReceipt{}, merr
		}
		if block != "" {
			_ = st.SetState(id, sessioninbox.StateBlocked, block)
			_ = st.SetPaused(true)
			return sessioninbox.InboxReceipt{}, fmt.Errorf("%w: %s", sessioninbox.ErrInvalidState, block)
		}
		submit = sessioninbox.ApplyFrozenRefs(submit, bodies)
	}
	// Mark running only after we know we will attempt admission; on reject restore queued.
	_ = st.SetState(id, sessioninbox.StateRunning, "")
	c.inbox.mu.Lock()
	c.inbox.trackActive(id)
	c.inbox.mu.Unlock()

	display := env.DisplayText
	raw := env.RawText
	c.mu.Lock()
	busy := c.running || c.finishing || c.rotating || c.closed
	c.mu.Unlock()
	if busy {
		_ = st.SetState(id, sessioninbox.StateQueued, "")
		c.inbox.mu.Lock()
		c.inbox.untrackActive(id)
		c.inbox.mu.Unlock()
		return c.busyReceipt(id, st), nil
	}
	// The envelope has already been classified and its references materialized.
	// Start a prepared user turn directly: routing through Submit/SubmitDisplay
	// would parse the original @tokens again and mix live workspace bytes with
	// the enqueue-time snapshot.
	c.submitPreparedInboxTurn(display, submit, raw, env.Format)
	return sessioninbox.InboxReceipt{
		ItemID:      id,
		Disposition: sessioninbox.DispositionStarted,
		Capacity:    st.Snapshot().Capacity,
	}, nil
}

// submitPreparedInboxTurn starts an already-classified inbox envelope without
// interpreting slash commands, shell shortcuts, or @references a second time.
// Dynamic frozen bodies remain in the user turn and never alter the stable
// system/tool prefix.
func (c *Controller) submitPreparedInboxTurn(display, submit, raw, format string) {
	display = firstNonEmptyStr(display, submit)
	raw = firstNonEmptyStr(raw, submit)
	c.runGuarded(func(ctx context.Context) error {
		return c.runGoalLoopWithRawDisplay(c.withTurnFormat(ctx, strings.TrimSpace(format)), submit, raw, display)
	})
}

func (c *Controller) busyReceipt(id string, st *sessioninbox.Store) sessioninbox.InboxReceipt {
	c.mu.Lock()
	defer c.mu.Unlock()
	cap := st.Snapshot().Capacity
	switch {
	case c.closed:
		return sessioninbox.InboxReceipt{ItemID: id, Disposition: sessioninbox.DispositionRejectedClosed, Capacity: cap}
	case c.rotating:
		return sessioninbox.InboxReceipt{ItemID: id, Disposition: sessioninbox.DispositionRejectedRotating, Capacity: cap}
	default:
		return sessioninbox.InboxReceipt{ItemID: id, Disposition: sessioninbox.DispositionRejectedBusy, Capacity: cap}
	}
}

// onInboxTurnDone acknowledges durable completion of every active inbox item
// (running follow-up + all steers accepted this turn). Dispatch of the next
// item is deferred until the finishing window closes so admission is not
// rejected as busy.
func (c *Controller) onInboxTurnDone() {
	c.inbox.mu.Lock()
	ids := c.inbox.takeActive()
	st := c.inbox.store
	c.inbox.mu.Unlock()
	if st == nil || len(ids) == 0 {
		return
	}
	// Transcript snapshot is the durable receipt boundary for the whole set.
	if err := c.SnapshotActivity(); err != nil {
		slog.Warn("controller: inbox turn snapshot", "err", err)
		for _, id := range ids {
			_ = st.SetState(id, sessioninbox.StateUncertain, "turn completed but transcript snapshot failed")
		}
		_ = st.SetPaused(true)
		sessioninbox.NoteUncertain()
		return
	}
	for _, id := range ids {
		if err := st.AckDequeue(id); err != nil {
			// Already deleted or race: log and continue so remaining items still ack.
			slog.Warn("controller: inbox ack dequeue", "err", err, "id", id)
		}
	}
}

// onInboxUnappliedSteer keeps accepted-but-unapplied steers for inspection.
func (c *Controller) onInboxUnappliedSteer(itemID string) {
	if itemID == "" {
		return
	}
	st, err := c.ensureInbox()
	if err != nil {
		return
	}
	_ = st.SetState(itemID, sessioninbox.StateUncertain, "steer accepted but unapplied before turn exit")
	_ = st.SetPaused(true)
	c.inbox.mu.Lock()
	c.inbox.untrackActive(itemID)
	c.inbox.mu.Unlock()
	sessioninbox.NoteUncertain()
}

// onInboxSteerConsumed marks steer_accepted → steer_consumed.
func (c *Controller) onInboxSteerConsumed(itemID string) {
	if itemID == "" {
		return
	}
	st, err := c.ensureInbox()
	if err != nil {
		return
	}
	_ = st.SetState(itemID, sessioninbox.StateSteerConsumed, "")
}

// maybeDispatchInbox admits the next FIFO item when idle, not paused, and no
// approval/ask UI is open.
func (c *Controller) maybeDispatchInbox() {
	c.inbox.mu.Lock()
	if c.inbox.dispatching {
		c.inbox.mu.Unlock()
		return
	}
	c.inbox.dispatching = true
	c.inbox.mu.Unlock()
	defer func() {
		c.inbox.mu.Lock()
		c.inbox.dispatching = false
		c.inbox.mu.Unlock()
	}()

	if c.PendingPrompt() {
		return
	}
	c.mu.Lock()
	busy := c.running || c.finishing || c.rotating || c.closed
	c.mu.Unlock()
	if busy {
		return
	}
	st, err := c.ensureInbox()
	if err != nil {
		return
	}
	meta, ok := st.NextQueued()
	if !ok {
		return
	}
	_, _ = c.TrySubmitInboxItem(meta.ID)
}

// TryEnqueueAndSteer is a convenience for frontends: durable steer then TrySteer.
func (c *Controller) TryEnqueueAndSteer(req InboxRequest) (sessioninbox.InboxReceipt, error) {
	req.Intent = sessioninbox.IntentSteer
	rec, err := c.EnqueueInbox(req)
	if err != nil {
		return rec, err
	}
	if rec.Idempotent {
		// Re-attempt steer on the existing item.
	}
	return c.TrySteerInboxItem(rec.ItemID)
}

// TryEnqueueFollowup durably queues a follow-up and may dispatch if idle.
func (c *Controller) TryEnqueueFollowup(req InboxRequest) (sessioninbox.InboxReceipt, error) {
	req.Intent = sessioninbox.IntentFollowup
	rec, err := c.EnqueueInbox(req)
	if err != nil {
		return rec, err
	}
	if !c.Running() {
		c.maybeDispatchInbox()
	}
	return rec, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// extractRefPathTokens is a lightweight @token scanner for freeze-at-enqueue.
// Full classification still uses ResolveRefs at consumption.
func extractRefPathTokens(line string) []string {
	var out []string
	seen := map[string]struct{}{}
	for i := 0; i < len(line); i++ {
		if line[i] != '@' {
			continue
		}
		j := i + 1
		for j < len(line) {
			ch := line[j]
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				break
			}
			j++
		}
		tok := strings.TrimSpace(line[i+1 : j])
		if tok == "" {
			continue
		}
		// Strip trailing punctuation commonly attached to refs.
		tok = strings.TrimRight(tok, ".,;:!?)]}>\"'")
		if tok == "" {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}
