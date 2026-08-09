package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/sessioninbox"
)

// enqueueViaInbox durably queues an inbound message on the controller's
// session inbox. Platform message IDs are used as idempotency keys.
func enqueueViaInbox(ctrl control.SessionAPI, msg InboundMessage, intent sessioninbox.InboxIntent) (sessioninbox.InboxReceipt, error) {
	if ctrl == nil {
		return sessioninbox.InboxReceipt{}, fmt.Errorf("no controller")
	}
	if ensurer, ok := ctrl.(interface{ EnsureSessionPath() }); ok {
		ensurer.EnsureSessionPath()
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return sessioninbox.InboxReceipt{}, sessioninbox.ErrEmpty
	}
	idem := strings.TrimSpace(msg.MessageID)
	req := control.InboxRequest{
		Intent:      intent,
		Display:     text,
		Raw:         text,
		Submit:      text,
		Source:      "bot",
		Idempotency: idem,
	}
	if intent == sessioninbox.IntentSteer {
		return ctrl.TryEnqueueAndSteer(req)
	}
	return ctrl.TryEnqueueFollowup(req)
}

// collectAppend tries to append text into the last queued follow-up blob within
// the debounce window. Falls back to a new enqueue.
func collectAppend(ctrl control.SessionAPI, msg InboundMessage, debounce time.Duration) (sessioninbox.InboxReceipt, error) {
	if ctrl == nil {
		return sessioninbox.InboxReceipt{}, fmt.Errorf("no controller")
	}
	snap := ctrl.InboxSnapshot()
	// Find last queued follow-up.
	var last *sessioninbox.InboxItemMeta
	for i := len(snap.Items) - 1; i >= 0; i-- {
		it := snap.Items[i]
		if it.State == sessioninbox.StateQueued && it.Intent == sessioninbox.IntentFollowup {
			last = &snap.Items[i]
			break
		}
	}
	text := strings.TrimSpace(msg.Text)
	if last != nil && debounce > 0 && time.Since(last.UpdatedAt) < debounce {
		_, env, err := ctrl.ReadInboxItem(last.ID)
		if err == nil {
			merged := env.SubmitText
			if merged != "" && text != "" {
				merged = merged + "\n" + text
			} else if text != "" {
				merged = text
			}
			if _, err := ctrl.UpdateInboxItem(last.ID, merged, merged, merged); err == nil {
				return sessioninbox.InboxReceipt{
					ItemID:      last.ID,
					Disposition: sessioninbox.DispositionQueuedFollowup,
					Position:    snap.Capacity.Items,
					Paused:      snap.Paused,
					Capacity:    snap.Capacity,
				}, nil
			}
		}
	}
	return enqueueViaInbox(ctrl, msg, sessioninbox.IntentFollowup)
}

// interruptEnqueue cancels the current turn and moves a new item to the front.
func interruptEnqueue(ctrl control.SessionAPI, msg InboundMessage) (sessioninbox.InboxReceipt, error) {
	if ctrl == nil {
		return sessioninbox.InboxReceipt{}, fmt.Errorf("no controller")
	}
	ctrl.Cancel()
	rec, err := enqueueViaInbox(ctrl, msg, sessioninbox.IntentFollowup)
	if err != nil {
		return rec, err
	}
	// Move to front (index 0) so it runs next; do not delete existing queue.
	if err := ctrl.MoveInboxItem(rec.ItemID, 0); err != nil {
		slog.Warn("bot: move interrupt item to front", "err", err)
	}
	return rec, nil
}

// formatQueuedReceipt is the user-visible durable queue confirmation.
func formatQueuedReceipt(rec sessioninbox.InboxReceipt) string {
	return fmt.Sprintf("已持久排队 #%s", shortItemID(rec.ItemID))
}

func shortItemID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// warnDeprecatedQueueDrop logs once when an old drop policy is still configured.
func warnDeprecatedQueueDrop(drop string) {
	switch NormalizeQueueDrop(drop) {
	case QueueDropOld, QueueDropSummarize:
		slog.Warn("bot: queue_drop is deprecated; capacity rejections no longer drop old messages", "drop", drop)
	}
}

// handleQueueInboxCommand extends /queue with durable inbox management.
// Returns handled=false for mode-switch forms of /queue.
func (gw *BotGateway) handleQueueInboxCommand(ctx context.Context, key string, msg InboundMessage) (string, bool) {
	_ = ctx
	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		return "", false
	}
	sub := strings.ToLower(parts[1])
	switch sub {
	case "list", "ls", "show", "delete", "rm", "move", "pause", "resume", "retry", "refresh":
		// ok
	default:
		return "", false
	}
	api := gw.sessionAPI(key)
	if api == nil {
		return "当前没有可管理的会话队列。", true
	}
	// Group chats: only the same session initiator or admins may read bodies.
	// Mode-level admin gate is enforced by requireCommandRole on sensitive ops.
	switch sub {
	case "list", "ls":
		snap := api.InboxSnapshot()
		if len(snap.Items) == 0 {
			return "inbox empty" + pausedSuffix(snap.Paused), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "inbox items=%d", len(snap.Items))
		if snap.Paused {
			b.WriteString(" paused")
		}
		b.WriteByte('\n')
		limit := min(len(snap.Items), 15)
		for i := 0; i < limit; i++ {
			it := snap.Items[i]
			fmt.Fprintf(&b, "%d. [%s/%s] %s #%s\n", i+1, it.Intent, it.State, it.Preview, shortItemID(it.ID))
		}
		return strings.TrimRight(b.String(), "\n"), true
	case "show":
		if len(parts) < 3 {
			return "用法: /queue show <n|id>", true
		}
		id, err := resolveBotInboxRef(api, parts[2])
		if err != nil {
			return err.Error(), true
		}
		_, env, err := api.ReadInboxItem(id)
		if err != nil {
			return "show: " + err.Error(), true
		}
		return env.SubmitText, true
	case "delete", "rm":
		if len(parts) < 3 {
			return "用法: /queue delete <n|id>", true
		}
		id, err := resolveBotInboxRef(api, parts[2])
		if err != nil {
			return err.Error(), true
		}
		if err := api.DeleteInboxItem(id); err != nil {
			return "delete: " + err.Error(), true
		}
		return "deleted #" + shortItemID(id), true
	case "move":
		if len(parts) < 4 {
			return "用法: /queue move <n|id> <to>", true
		}
		id, err := resolveBotInboxRef(api, parts[2])
		if err != nil {
			return err.Error(), true
		}
		var to int
		if _, err := fmt.Sscanf(parts[3], "%d", &to); err != nil {
			return "move: bad index", true
		}
		if err := api.MoveInboxItem(id, to-1); err != nil {
			return "move: " + err.Error(), true
		}
		return "moved #" + shortItemID(id), true
	case "pause":
		if err := api.SetInboxPaused(true); err != nil {
			return err.Error(), true
		}
		return "inbox paused", true
	case "resume":
		if err := api.SetInboxPaused(false); err != nil {
			return err.Error(), true
		}
		return "inbox resumed", true
	case "retry":
		if len(parts) < 3 {
			return "用法: /queue retry <n|id>", true
		}
		id, err := resolveBotInboxRef(api, parts[2])
		if err != nil {
			return err.Error(), true
		}
		if err := api.RetryInboxItem(id); err != nil {
			return err.Error(), true
		}
		return "retry #" + shortItemID(id), true
	case "refresh":
		if len(parts) < 3 {
			return "用法: /queue refresh <n|id>", true
		}
		id, err := resolveBotInboxRef(api, parts[2])
		if err != nil {
			return err.Error(), true
		}
		if err := api.RefreshInboxReferences(id); err != nil {
			return err.Error(), true
		}
		return "refs refreshed #" + shortItemID(id), true
	}
	return "", false
}

func pausedSuffix(paused bool) string {
	if paused {
		return " (paused)"
	}
	return ""
}

func resolveBotInboxRef(api control.SessionAPI, ref string) (string, error) {
	snap := api.InboxSnapshot()
	var n int
	if _, err := fmt.Sscanf(ref, "%d", &n); err == nil && n >= 1 && n <= len(snap.Items) {
		return snap.Items[n-1].ID, nil
	}
	for _, it := range snap.Items {
		if it.ID == ref || strings.HasPrefix(it.ID, ref) {
			return it.ID, nil
		}
	}
	return "", fmt.Errorf("unknown inbox item %q", ref)
}
