// Coalesces text/reasoning stream deltas into one flush per animation frame.
// Non-text events must drain() first so causal ordering is preserved.
//
// The rAF flush is backed by a stall timer: when requestAnimationFrame stops
// firing (WebView2 window minimized/occluded, saturated main thread), deltas
// would otherwise pile up in the buffer until a non-stream event calls drain()
// — the transcript freezes on "thinking…" and the whole reply appears only
// after the user hits Stop. With the timer, the flush still happens while
// stalled; rAF simply wins the race whenever frames are being produced, so
// the visible path keeps its one-flush-per-frame behavior.

type Flush<T> = (batch: T[]) => void;

interface BatchHandle<T> {
  push: (item: T) => void;
  drain: () => void;
  size: () => number;
}

// Fallback flush interval used only while rAF is not firing. 200ms is far
// longer than any healthy frame interval, so the timer never fires in the
// common visible path where rAF is cancelled first.
const STALL_TIMEOUT_MS = 200;

export function createRafBatch<T>(flush: Flush<T>): BatchHandle<T> {
  let buffer: T[] = [];
  let scheduled: number | null = null; // rAF id; 1 = microtask fallback (no rAF)
  let stallTimer: ReturnType<typeof setTimeout> | null = null;

  const clearScheduled = () => {
    if (scheduled !== null && scheduled !== 1 && typeof cancelAnimationFrame !== "undefined") {
      cancelAnimationFrame(scheduled);
    }
    scheduled = null;
  };

  const clearStallTimer = () => {
    if (stallTimer !== null) {
      clearTimeout(stallTimer);
      stallTimer = null;
    }
  };

  const run = () => {
    clearScheduled();
    clearStallTimer();
    // Snapshot + clear before flushing so a re-entrant push() lands next frame.
    const out = buffer;
    buffer = [];
    if (out.length > 0) flush(out);
  };

  const arm = () => {
    if (scheduled === null && typeof requestAnimationFrame !== "undefined") {
      scheduled = requestAnimationFrame(run);
    } else if (scheduled === null) {
      // No rAF (SSR / JSDOM) — fall back to a microtask.
      scheduled = 1;
      Promise.resolve().then(run);
    }
    if (stallTimer === null && typeof setTimeout !== "undefined") {
      stallTimer = setTimeout(run, STALL_TIMEOUT_MS);
    }
  };

  const handle: BatchHandle<T> = {
    push(item: T) {
      buffer.push(item);
      if (scheduled === null) arm();
    },
    drain() {
      clearScheduled();
      clearStallTimer();
      run();
    },
    size() {
      return buffer.length;
    },
  };
  return handle;
}
