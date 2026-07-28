import { useCallback, useEffect, useRef, useState } from "react";
import {
  AlertCircle,
  ChevronDown,
  ChevronRight,
  Clock,
  List,
  Loader2,
  RotateCw,
  X,
  XCircle,
} from "lucide-react";
import { app } from "../lib/bridge";
import type { TaskEvent, TaskSnapshot } from "../lib/types";

// --- helpers ---

const STATE_CONFIG: Record<
  string,
  { label: string; color: string; dot: string }
> = {
  queued: { label: "Queued", color: "#6b7280", dot: "⚪" },
  running: { label: "Running", color: "#3b82f6", dot: "🔵" },
  waiting: { label: "Waiting", color: "#f59e0b", dot: "🟡" },
  succeeded: { label: "Succeeded", color: "#22c55e", dot: "🟢" },
  failed: { label: "Failed", color: "#ef4444", dot: "🔴" },
  cancelled: { label: "Cancelled", color: "#9ca3af", dot: "⏹️" },
  stale: { label: "Stale", color: "#d4d4d8", dot: "⬜" },
};

function stateConfig(state: string) {
  return STATE_CONFIG[state] ?? { label: state, color: "#6b7280", dot: "❓" };
}

function safeStateClass(state: string): string {
  // Sanitize state for use in CSS class names — only allow word chars.
  return state.replace(/[^a-zA-Z0-9_-]/g, "_");
}

function elapsed(iso: string): string {
  if (!iso) return "—";
  const ms = Date.now() - new Date(iso).getTime();
  if (isNaN(ms) || ms < 0) return "—";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h`;
}

function shortID(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function eventSummary(ev: TaskEvent): string {
  if (ev.error_code) return `Error: ${ev.error_code}`;
  switch (ev.event_type) {
    case "state_change":
      return `State → ${ev.state}`;
    case "error":
      return ev.error_summary || "Error";
    default:
      return ev.event_type;
  }
}

// --- component ---

const POLL_INTERVAL_MS = 5000;

export function TaskMonitorPanel({ onClose }: { onClose?: () => void }) {
  const [tasks, setTasks] = useState<TaskSnapshot[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [open, setOpen] = useState(false);
  const [actionTask, setActionTask] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionMessage, setActionMessage] = useState<string | null>(null);

  // Per-task event state
  const [taskEvents, setTaskEvents] = useState<Map<string, TaskEvent[]>>(
    () => new Map(),
  );
  const [eventsLoading, setEventsLoading] = useState<Set<string>>(new Set());
  const [eventsError, setEventsError] = useState<Map<string, string>>(
    () => new Map(),
  );
  const eventCursors = useRef<Map<string, number>>(new Map());

  const fetchTasks = useCallback(async () => {
    try {
      setError(null);
      const list = await app.ListTasks();
      setTasks(list ?? []);
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  // Fetch events for a single task, using afterSequence for incremental load.
  const fetchEvents = useCallback(async (taskID: string) => {
    setEventsLoading((prev) => new Set(prev).add(taskID));
    setEventsError((prev) => {
      const next = new Map(prev);
      next.delete(taskID);
      return next;
    });
    try {
      const cursor = eventCursors.current.get(taskID) ?? 0;
      const events = await app.ListTaskEvents(taskID, cursor);
      if (events.length > 0) {
        setTaskEvents((prev) => {
          const next = new Map(prev);
          const existing = next.get(taskID) ?? [];
          // Merge, deduplicate by sequence
          const seen = new Set(existing.map((e) => e.sequence));
          const merged = [...existing, ...events.filter((e) => !seen.has(e.sequence))];
          merged.sort((a, b) => a.sequence - b.sequence);
          next.set(taskID, merged);
          return next;
        });
        // Update cursor to the max sequence
        const maxSeq = events.reduce(
          (max, e) => Math.max(max, e.sequence),
          cursor,
        );
        eventCursors.current.set(taskID, maxSeq);
      }
    } catch (e) {
      setEventsError((prev) => {
        const next = new Map(prev);
        next.set(taskID, String(e));
        return next;
      });
    } finally {
      setEventsLoading((prev) => {
        const next = new Set(prev);
        next.delete(taskID);
        return next;
      });
    }
  }, []);

  // Initial fetch + periodic polling
  useEffect(() => {
    fetchTasks();
    const interval = setInterval(() => {
      fetchTasks();
      // Also refresh events for expanded tasks
      expanded.forEach((id) => {
        fetchEvents(id);
      });
    }, POLL_INTERVAL_MS);
    return () => clearInterval(interval);
  }, [fetchTasks, fetchEvents, expanded]);

  const toggleTask = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
        // Load events on first expand
        if (!taskEvents.has(id)) {
          fetchEvents(id);
        }
      }
      return next;
    });
  };

  const controlTask = async (task: TaskSnapshot, action: "stop" | "cancel" | "resume" | "open") => {
    if ((action === "stop" || action === "cancel") && !window.confirm(`${action === "stop" ? "Stop" : "Cancel"} task ${task.task_id}?`)) return;
    setActionTask(task.task_id);
    setActionError(null);
    setActionMessage(null);
    try {
      const result = action === "stop"
        ? await app.StopTask(task.task_id, task.version, "desktop request", `desktop-${action}-${task.task_id}-${task.version}`)
        : action === "cancel"
          ? await app.CancelTask(task.task_id, task.version, "desktop request", `desktop-${action}-${task.task_id}-${task.version}`)
          : action === "resume"
            ? await app.ResumeTask(task.task_id, task.version, `desktop-${action}-${task.task_id}-${task.version}`)
            : await app.OpenTaskSession(task.task_id);
      if (result.error) {
        setActionError(`${result.error.code}: ${result.error.message}`);
      } else if (action === "open") {
        setActionMessage(`Session: ${result.session_id ?? "—"}`);
      } else {
        setActionMessage(result.idempotent ? "Already applied" : "Task updated");
        await fetchTasks();
      }
    } catch (e) {
      setActionError(String(e));
    } finally {
      setActionTask(null);
    }
  };

  const sorted = [...tasks].sort(
    (a, b) =>
      new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime(),
  );

  return (
    <div className="taskmonitor">
      <div className="taskmonitor__head">
        <button
          className="taskmonitor__toggle"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          aria-label={open ? "Collapse tasks" : "Expand tasks"}
        >
          {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </button>
        <span className="taskmonitor__title">Tasks</span>
        <span className="taskmonitor__count">{tasks.length}</span>
        <button
          className="taskmonitor__refresh"
          onClick={() => {
            setLoading(true);
            fetchTasks();
          }}
          title="Refresh"
          aria-label="Refresh tasks"
        >
          <RotateCw size={12} />
        </button>
        {onClose && (
          <button
            className="taskmonitor__close"
            onClick={onClose}
            title="Close"
            aria-label="Close task panel"
          >
            <X size={14} />
          </button>
        )}
      </div>

      {open && (
        <div className="taskmonitor__body">
          {actionError && <div className="taskmonitor__state taskmonitor__state--error">{actionError}</div>}
          {actionMessage && <div className="taskmonitor__state">{actionMessage}</div>}
          {loading && (
            <div className="taskmonitor__state">
              <Loader2 size={16} className="taskmonitor__spinner" />
              <span>Loading...</span>
            </div>
          )}

          {error && (
            <div className="taskmonitor__state taskmonitor__state--error">
              <AlertCircle size={16} />
              <span>{error}</span>
            </div>
          )}

          {!loading && !error && sorted.length === 0 && (
            <div className="taskmonitor__state taskmonitor__state--empty">
              <Clock size={16} />
              <span>No background tasks</span>
            </div>
          )}

          {!loading &&
            sorted.map((t) => {
              const cfg = stateConfig(t.state);
              const isOpen = expanded.has(t.task_id);
              const terminal =
                t.state === "succeeded" ||
                t.state === "failed" ||
                t.state === "cancelled" ||
                t.state === "stale";
              const evs = taskEvents.get(t.task_id) ?? [];
              const evLoading = eventsLoading.has(t.task_id);
              const evError = eventsError.get(t.task_id);

              return (
                <div
                  key={t.task_id}
                  className={`taskmonitor__task taskmonitor__task--${safeStateClass(t.state)}`}
                >
                  <div className="taskmonitor__task-head">
                    <button
                      className="taskmonitor__expand"
                      onClick={() => toggleTask(t.task_id)}
                      aria-expanded={isOpen}
                      aria-label={`Task ${shortID(t.task_id)} — ${cfg.label}`}
                    >
                      <span
                        className="taskmonitor__dot"
                        style={{ color: cfg.color }}
                      >
                        {cfg.dot}
                      </span>
                      <span className="taskmonitor__id">
                        {shortID(t.task_id)}
                      </span>
                      <span
                        className="taskmonitor__badge"
                        style={{
                          backgroundColor: cfg.color + "18",
                          color: cfg.color,
                        }}
                      >
                        {cfg.label}
                      </span>
                      {terminal && (
                        <XCircle size={12} className="taskmonitor__terminal" />
                      )}
                      <span className="taskmonitor__time">
                        {elapsed(t.updated_at)}
                      </span>
                      {isOpen ? (
                        <ChevronDown size={12} />
                      ) : (
                        <ChevronRight size={12} />
                      )}
                    </button>
                  </div>

                  {isOpen && (
                    <div className="taskmonitor__detail">
                      <dl>
                        <dt>Task ID</dt>
                        <dd>{t.task_id}</dd>
                        <dt>Session</dt>
                        <dd>{t.session_id || "—"}</dd>
                        <dt>State</dt>
                        <dd>{t.state}</dd>
                        <dt>Updated</dt>
                        <dd>{new Date(t.updated_at).toLocaleString()}</dd>
                        {t.error_code && (
                          <>
                            <dt>Error Code</dt>
                            <dd className="taskmonitor__err">{t.error_code}</dd>
                          </>
                        )}
                        {t.error_summary && (
                          <>
                            <dt>Summary</dt>
                            <dd className="taskmonitor__err-summary">
                              {t.error_summary}
                            </dd>
                          </>
                        )}
                      </dl>

                      {/* Events section */}
                      <div className="taskmonitor__events">
                        <div className="taskmonitor__events-head">
                          <List size={12} />
                          <span>Recent Events</span>
                          {evs.length > 0 && (
                            <span className="taskmonitor__events-count">
                              {evs.length}
                            </span>
                          )}
                        </div>

                        {evLoading && evs.length === 0 && (
                          <div className="taskmonitor__state">
                            <Loader2
                              size={12}
                              className="taskmonitor__spinner"
                            />
                            <span>Loading events...</span>
                          </div>
                        )}

                        {evError && (
                          <div className="taskmonitor__state taskmonitor__state--error">
                            <AlertCircle size={12} />
                            <span>{evError}</span>
                          </div>
                        )}

                        {!evLoading && !evError && evs.length === 0 && (
                          <div className="taskmonitor__state taskmonitor__state--empty">
                            <span>No events yet</span>
                          </div>
                        )}

                        {evs.length > 0 && (
                          <ul className="taskmonitor__event-list">
                            {evs.map((ev) => (
                              <li
                                key={ev.sequence}
                                className="taskmonitor__event"
                              >
                                <span className="taskmonitor__event-seq">
                                  #{ev.sequence}
                                </span>
                                <span className="taskmonitor__event-type">
                                  {eventSummary(ev)}
                                </span>
                                <span className="taskmonitor__event-time">
                                  {new Date(ev.timestamp).toLocaleTimeString()}
                                </span>
                              </li>
                            ))}
                          </ul>
                        )}
                      </div>
                      <div className="taskmonitor__actions">
                        {(t.state === "queued" || t.state === "running" || t.state === "waiting") && (
                          <>
                            <button disabled={actionTask === t.task_id} onClick={() => void controlTask(t, "stop")}>Stop</button>
                            <button disabled={actionTask === t.task_id} onClick={() => void controlTask(t, "cancel")}>Cancel</button>
                          </>
                        )}
                        {(t.state === "failed" || t.state === "stale") && (
                          <button disabled={actionTask === t.task_id} onClick={() => void controlTask(t, "resume")}>Resume</button>
                        )}
                        <button disabled={actionTask === t.task_id} onClick={() => void controlTask(t, "open")}>Open Session</button>
                      </div>
                    </div>
                  )}
                </div>
              );
            })}
        </div>
      )}
    </div>
  );
}
