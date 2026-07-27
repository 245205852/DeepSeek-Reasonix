import { useCallback, useEffect, useState } from "react";
import {
  AlertCircle,
  ChevronDown,
  ChevronRight,
  Clock,
  Loader2,
  RotateCw,
  X,
  XCircle,
} from "lucide-react";
import { app } from "../lib/bridge";
import type { TaskSnapshot } from "../lib/types";

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

function elapsed(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (ms < 0) return "0s";
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

// --- component ---

export function TaskMonitorPanel({ onClose }: { onClose?: () => void }) {
  const [tasks, setTasks] = useState<TaskSnapshot[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [open, setOpen] = useState(false);

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

  useEffect(() => {
    fetchTasks();
    const interval = setInterval(fetchTasks, 5000);
    return () => clearInterval(interval);
  }, [fetchTasks]);

  const toggleTask = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const sorted = [...tasks].sort(
    (a, b) =>
      new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime(),
  );

  return (
    <div className="taskmonitor">
      <button
        className="taskmonitor__head"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        <span className="taskmonitor__title">Tasks</span>
        <span className="taskmonitor__count">{tasks.length}</span>
        <button
          className="taskmonitor__refresh"
          onClick={(e) => {
            e.stopPropagation();
            setLoading(true);
            fetchTasks();
          }}
          title="Refresh"
        >
          <RotateCw size={12} />
        </button>
        {onClose && (
          <button className="taskmonitor__close" onClick={onClose} title="Close">
            <X size={14} />
          </button>
        )}
      </button>

      {open && (
        <div className="taskmonitor__body">
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

              return (
                <div
                  key={t.task_id}
                  className={`taskmonitor__task taskmonitor__task--${t.state}`}
                >
                  <button
                    className="taskmonitor__task-head"
                    onClick={() => toggleTask(t.task_id)}
                    aria-expanded={isOpen}
                    aria-label={`Task ${shortID(t.task_id)} — ${cfg.label}`}
                  >
                    <span className="taskmonitor__dot" style={{ color: cfg.color }}>
                      {cfg.dot}
                    </span>
                    <span className="taskmonitor__id">{shortID(t.task_id)}</span>
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
