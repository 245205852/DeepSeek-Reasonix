import { useCallback, useEffect, useRef, useState } from "react";
import { app, onRemoteTabEvent, onRemoteTabState } from "./bridge";
import type { CancelOutcome } from "./inboxCancel";
import { initialState, reducer, type State } from "./useController";
import type { CheckpointMeta, CollaborationMode, EffortInfo, GoalStatus, HistoryMessage, RemoteTabStateValue, TabMeta, ToolApprovalMode, WireEvent } from "./types";
import type { RemoteAskAnswer } from "./remoteTypes";

// The remote session reuses the local transcript pipeline end to end: serve
// frames share the agent event wire form, so they run through the same
// reducer that drives local tabs, and /history hydrates through the same
// history action. The surface and composer therefore consume exactly the
// shapes the local UI consumes.

// remoteStatusToAction maps the serve's raw /status payload onto the shared
// backend_status action so the remote surface reuses the local tab's running
// reconciliation (including its staleness guards). The serve reports the
// fields it knows; the rest stay undefined and the reducer keeps prior values.
function remoteStatusToAction(status: unknown, snapshotAt: number) {
  const raw = (status ?? null) as { running?: unknown; pendingPrompt?: unknown; backgroundJobs?: unknown; cancelRequested?: unknown; cancellable?: unknown } | null;
  return {
    type: "backend_status" as const,
    running: raw?.running === true,
    pendingPrompt: raw?.pendingPrompt === undefined ? undefined : raw.pendingPrompt === true,
    backgroundJobs: typeof raw?.backgroundJobs === "number" ? raw.backgroundJobs : undefined,
    cancelRequested: raw?.cancelRequested === undefined ? undefined : raw.cancelRequested === true,
    cancellable: raw?.cancellable === undefined ? (raw?.running === true) : raw.cancellable === true,
    snapshotAt,
  };
}

// RemoteSessionApi is the surface-facing contract of useRemoteSession.
export interface RemoteSessionApi {
  state: RemoteTabStateValue;
  error: string;
  transcript: State;
  hydrated: boolean;
  running: boolean;
  /** The serve's label for the active model, for the composer capsule. */
  modelLabel: string;
  composerProfile?: {
    collaborationMode: CollaborationMode;
    toolApprovalMode: ToolApprovalMode;
    goal: string;
    goalStatus?: GoalStatus;
  };
  effort?: EffortInfo;
  /** Changes whenever the tab adopts a new/reconnected Serve session snapshot. */
  surfaceGeneration: number;
  promptError: string;
  submit: (text: string) => Promise<void>;
  cancelTurn: () => Promise<void>;
  approve: (callId: string, decision: string) => Promise<void>;
  answer: (callId: string, answers: RemoteAskAnswer[]) => Promise<void>;
  rewind: (turn: number, scope: string) => Promise<void>;
  setEffort: (level: string) => Promise<void>;
  pauseGoal: () => Promise<void>;
  resumeGoal: () => Promise<void>;
  steer: (input: string) => Promise<void>;
}

type RemoteStatus = {
  label?: unknown;
  plan?: unknown;
  toolApprovalMode?: unknown;
  goal?: unknown;
  goalStatus?: unknown;
  effort?: unknown;
};

function isAuthoritativeRemoteStatus(status: unknown): status is RemoteStatus {
  if (!status || typeof status !== "object" || Array.isArray(status)) return false;
  const raw = status as RemoteStatus;
  return typeof raw.plan === "boolean"
    && (raw.toolApprovalMode === "ask" || raw.toolApprovalMode === "auto" || raw.toolApprovalMode === "yolo")
    && typeof raw.goal === "string";
}

function remoteComposerState(status: unknown) {
  const raw = (status ?? null) as RemoteStatus | null;
  const goal = typeof raw?.goal === "string" ? raw.goal.trim() : "";
  const toolApprovalMode: ToolApprovalMode = raw?.toolApprovalMode === "auto" || raw?.toolApprovalMode === "yolo"
    ? raw.toolApprovalMode
    : "ask";
  const rawGoalStatus = raw?.goalStatus;
  const goalStatus: GoalStatus | undefined = rawGoalStatus === "running" || rawGoalStatus === "complete"
    || rawGoalStatus === "blocked" || rawGoalStatus === "stopped" ? rawGoalStatus : undefined;
  const effort = raw?.effort as Partial<EffortInfo> | undefined;
  return {
    modelLabel: typeof raw?.label === "string" ? raw.label : "",
    composerProfile: {
      collaborationMode: goal ? "goal" as const : raw?.plan === true ? "plan" as const : "normal" as const,
      toolApprovalMode,
      goal,
      goalStatus,
    },
    effort: effort && typeof effort.supported === "boolean"
      ? {
          supported: effort.supported,
          current: typeof effort.current === "string" ? effort.current : "auto",
          default: typeof effort.default === "string" ? effort.default : "",
          levels: Array.isArray(effort.levels) ? effort.levels.filter((level): level is string => typeof level === "string") : [],
        }
      : undefined,
  };
}

function remoteCheckpoints(value: unknown): CheckpointMeta[] {
  if (!Array.isArray(value)) return [];
  return value.flatMap((entry) => {
    const raw = (entry ?? null) as Record<string, unknown> | null;
    if (!raw || typeof raw.turn !== "number" || !Number.isFinite(raw.turn)) return [];
    const files = Array.isArray(raw.files)
      ? raw.files.filter((path): path is string => typeof path === "string")
      : [];
    const numericFileCount = typeof raw.files === "number" ? raw.files : raw.fileCount;
    const fileCount = typeof numericFileCount === "number" && Number.isFinite(numericFileCount)
      ? Math.max(0, numericFileCount)
      : files.length;
    return [{
      turn: raw.turn,
      prompt: typeof raw.prompt === "string" ? raw.prompt : "",
      files,
      fileCount,
      filesTruncated: raw.filesTruncated === true,
      turnFileCount: typeof raw.turnFileCount === "number" ? raw.turnFileCount : undefined,
      time: typeof raw.time === "number" ? raw.time : 0,
      canCode: raw.canCode === true,
      canConversation: raw.canConversation === true,
      coverage: typeof raw.coverage === "string" ? raw.coverage : undefined,
      coverageGaps: Array.isArray(raw.coverageGaps)
        ? raw.coverageGaps.filter((gap): gap is string => typeof gap === "string")
        : undefined,
      expiredFilePayload: raw.expiredFilePayload === true,
      activeWriters: typeof raw.activeWriters === "number" ? raw.activeWriters : undefined,
      legacy: raw.legacy === true,
      canUndoFiles: raw.canUndoFiles === true,
      disabledReason: typeof raw.disabledReason === "string" ? raw.disabledReason : undefined,
    }];
  });
}

export function useRemoteComposer(
  session: RemoteSessionApi,
  showToast: (message: string, level: "error") => void,
) {
  const onSend = useCallback(async (displayText: string, submitText = displayText) => {
    const text = (submitText || displayText).trim();
    if (!text) return;
    try {
      await session.submit(text);
    } catch (error) {
      showToast(error instanceof Error ? error.message : String(error), "error");
    }
  }, [session, showToast]);
  const onCancel = useCallback(async (_queuedItemIDs?: string[]): Promise<CancelOutcome> => {
    void session.cancelTurn().catch((error) => {
      showToast(error instanceof Error ? error.message : String(error), "error");
    });
    return { discardedItemIds: [] };
  }, [session, showToast]);
  return { onSend, onCancel };
}

export function useActiveRemoteSession(
  activeTab: TabMeta | undefined,
  showToast: (message: string, level: "error") => void,
) {
  const active = Boolean(activeTab?.remote);
  const session = useRemoteSession(active && activeTab ? activeTab.id : undefined, activeTab?.remoteState);
  const composer = useRemoteComposer(session, showToast);
  return { active, session, ready: active && session.state === "ready" && session.hydrated && Boolean(session.composerProfile), ...composer };
}

export function useRemoteSession(tabId: string | undefined, initial?: RemoteTabStateValue): RemoteSessionApi {
  const [state, setState] = useState<RemoteTabStateValue>(initial ?? "connecting");
  const [error, setError] = useState("");
  const [transcript, setTranscript] = useState<State>(initialState);
  const [modelLabel, setModelLabel] = useState("");
  const [composerProfile, setComposerProfile] = useState<RemoteSessionApi["composerProfile"]>();
  const [effort, setEffortInfo] = useState<EffortInfo>();
  const [surfaceGeneration, setSurfaceGeneration] = useState(0);
  const [promptError, setPromptError] = useState("");
  const [hydrated, setHydrated] = useState(false);
  const hydratedRef = useRef(false);
  const hydratingRef = useRef(false);
  const bufferedEventsRef = useRef<WireEvent[]>([]);
  const hydrateRef = useRef<((force?: boolean) => Promise<void>) | null>(null);

  const applyRemoteStatus = useCallback((status: unknown) => {
    if (!isAuthoritativeRemoteStatus(status)) return;
    const next = remoteComposerState(status);
    setModelLabel(next.modelLabel);
    setComposerProfile(next.composerProfile);
    setEffortInfo(next.effort);
  }, []);

  useEffect(() => {
    if (!tabId) return;
    // A restored disconnected shell seeds its state from the meta; live state
    // then flows through remote-tab:{id}:state events once a connect begins.
    const start = initial ?? "connecting";
    setState(start);
    setError("");
    setPromptError("");
    setTranscript(initialState);
    setModelLabel("");
    setComposerProfile(undefined);
    setEffortInfo(undefined);
    hydratedRef.current = false;
    hydratingRef.current = false;
    bufferedEventsRef.current = [];
    setHydrated(false);
    let cancelled = false;
    let hydratePromise: Promise<void> | null = null;
    // Never start the snapshot retry loop on a shell with no connection: the
    // ready transition triggers the first hydration instead. (initial is
    // deliberately not a dependency — only the mount-time snapshot matters.)
    const skipHydrate = start === "disconnected";

    // Hydrate from the snapshot; retry through the connecting window so a
    // late backend never leaves the surface empty. A forced run re-syncs
    // after a session reset or a reconnect: the snapshot reflects whatever
    // session the serve now holds.
    const hydrate = (force = false) => {
      if (force) {
        hydratedRef.current = false;
        setHydrated(false);
      }
      return hydrateLoop();
    };
    const hydrateLoop = async () => {
      if (hydratePromise) return hydratePromise;
      hydratingRef.current = true;
      hydratePromise = (async () => {
        for (let attempt = 0; attempt < 60 && !cancelled && !hydratedRef.current; attempt++) {
          try {
            const snap = await app.RemoteTabSnapshot(tabId);
            if (cancelled) return;
            const messages = Array.isArray(snap.history) ? (snap.history as HistoryMessage[]) : [];
            // /status is optional in the aggregate snapshot for non-composer
            // consumers, but the remote composer must not submit with guessed
            // plan/approval/goal settings. Fetch it explicitly if the optional
            // member missed; a failure keeps hydration in the retry loop.
            const status = snap.status ?? await app.RemoteTabStatus(tabId);
            if (cancelled) return;
            if (!isAuthoritativeRemoteStatus(status)) throw new Error("remote status is incomplete");
            hydratedRef.current = true;
            setHydrated(true);
            setSurfaceGeneration((generation) => generation + 1);
            applyRemoteStatus(status);
            const checkpoints = remoteCheckpoints(snap.checkpoints);
            const replay = [
              ...(Array.isArray(snap.pendingEvents) ? snap.pendingEvents : []),
              ...bufferedEventsRef.current,
            ] as WireEvent[];
            bufferedEventsRef.current = [];
            hydratingRef.current = false;
            setTranscript((s) => {
              let next = reducer(s, { type: "history", messages });
              next = reducer(next, { type: "checkpoints", checkpoints });
              // Hydrate doubles as the post-reconnect running reconciliation:
              // whatever the serve reports about its current state lands now,
              // not only after the next watchdog tick.
              next = reducer(next, remoteStatusToAction(status, Date.now()));
              const seenPrompts = new Set<string>();
              for (const event of replay) {
                const promptId = event.kind === "approval_request"
                  ? event.approval?.id
                  : event.kind === "ask_request" ? event.ask?.id : undefined;
                const promptKey = promptId ? `${event.kind}:${promptId}` : "";
                if (promptKey && seenPrompts.has(promptKey)) continue;
                if (promptKey) seenPrompts.add(promptKey);
                next = reducer(next, { type: "event", e: event });
              }
              return next;
            });
            return;
          } catch {
            // Executor form: the src tsconfig lib predates Promise.withResolvers.
            await new Promise<void>((resolve) => setTimeout(resolve, 500));
          }
        }
        hydratingRef.current = false;
        if (bufferedEventsRef.current.length > 0) {
          const buffered = bufferedEventsRef.current;
          bufferedEventsRef.current = [];
          setTranscript((current) => buffered.reduce(
            (next, event) => reducer(next, { type: "event", e: event }),
            current,
          ));
        }
      })();
      try {
        await hydratePromise;
      } finally {
        hydratePromise = null;
      }
    };
    hydrateRef.current = hydrate;
    if (!skipHydrate) void hydrateLoop();

    const offState = onRemoteTabState(tabId, (s) => {
      if (cancelled) return;
      setState(s.state);
      setError(s.error ?? "");
      if (s.state === "ready") {
        void hydrate(true);
      } else {
        // Leaving ready can only mean the serve connection dropped. A turn
        // that was running is now unobservable — stop the pill instead of
        // spinning forever on a turn_done that can never arrive.
        setTranscript((prev) => (prev.running || prev.turnActive ? reducer(prev, { type: "turn_interrupted" }) : prev));
      }
    });
    const offEvent = onRemoteTabEvent(tabId, (raw) => {
      if (cancelled) return;
      const event = (raw ?? {}) as WireEvent;
      if (hydratingRef.current) {
        bufferedEventsRef.current.push(event);
        return;
      }
      setTranscript((s) => reducer(s, { type: "event", e: event }));
    });
    return () => {
      cancelled = true;
      hydratingRef.current = false;
      bufferedEventsRef.current = [];
      if (hydrateRef.current === hydrate) hydrateRef.current = null;
      offState();
      offEvent();
    };
  }, [applyRemoteStatus, tabId]);

  // Running-state watchdog: while the pill claims a turn is running, poll the
  // serve's /status and feed it through the shared backend_status reducer.
  // This is the remote twin of the local tab's reconcile loop — a lost
  // turn_done frame (dropped SSE, slow-consumer drop, half-dead tunnel) then
  // clears within one tick instead of spinning forever.
  useEffect(() => {
    if (!tabId || !hydrated || state !== "ready" || !transcript.running) return;
    let cancelled = false;
    const reconcile = async () => {
      try {
        const status = await app.RemoteTabStatus(tabId);
        if (cancelled) return;
        applyRemoteStatus(status);
        setTranscript((s) => reducer(s, remoteStatusToAction(status, Date.now())));
      } catch {
        // Transient; the next tick retries.
      }
    };
    const timer = window.setInterval(reconcile, 30_000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [applyRemoteStatus, tabId, hydrated, state, transcript.running]);

  const submit = useCallback(async (text: string) => {
    if (!tabId) return;
    const trimmed = text.trim();
    if (!trimmed) return;
    // Optimistic user bubble, exactly like the local send path. seq rides
    // the reducer's counter; the submission id only needs uniqueness.
    const submissionId = `remote-${Date.now()}`;
    setTranscript((s) => reducer(s, { type: "user", text: trimmed, seq: s.seq, submissionId }));
    try {
      await app.SubmitRemoteTab(tabId, trimmed);
    } catch (e) {
      // Roll the optimistic running flag back — a refused/failed submit must
      // never leave the pill spinning (same contract as the local send path).
      const error = `Send failed: ${e instanceof Error ? e.message : String(e)}`;
      setTranscript((s) => reducer(s, { type: "send_failed", submissionId, error }));
      throw e;
    }
  }, [tabId]);

  const cancelTurn = useCallback(async () => {
    if (!tabId) return;
    await app.CancelRemoteTab(tabId);
  }, [tabId]);

  const approve = useCallback(async (callId: string, decision: string) => {
    if (!tabId) return;
    setPromptError("");
    try {
      await app.ApproveRemoteTab(tabId, callId, decision);
      setTranscript((s) => ({ ...s, approval: undefined }));
    } catch (error) {
      setPromptError(error instanceof Error ? error.message : String(error));
      throw error;
    }
  }, [tabId]);

  const answer = useCallback(async (callId: string, answers: RemoteAskAnswer[]) => {
    if (!tabId) return;
    setPromptError("");
    try {
      await app.AnswerRemoteTab(tabId, callId, answers);
      setTranscript((s) => ({ ...s, ask: undefined }));
    } catch (error) {
      setPromptError(error instanceof Error ? error.message : String(error));
      throw error;
    }
  }, [tabId]);

  const refreshSnapshot = useCallback(async () => {
    await hydrateRef.current?.(true);
  }, []);

  const rewind = useCallback(async (turn: number, scope: string) => {
    if (!tabId) return;
    setPromptError("");
    try {
      switch (scope) {
        case "fork":
          await app.ForkRemoteTab(tabId, turn, "");
          break;
        case "summ-from":
          await app.SummarizeRemoteTab(tabId, turn, "from");
          break;
        case "summ-upto":
          await app.SummarizeRemoteTab(tabId, turn, "upto");
          break;
        case "code":
        case "conversation":
        case "both":
          await app.RewindRemoteTab(tabId, String(turn), scope);
          break;
        default:
          throw new Error(`Unsupported remote rewind scope: ${scope}`);
      }
      await refreshSnapshot();
    } catch (error) {
      setPromptError(error instanceof Error ? error.message : String(error));
      throw error;
    }
  }, [refreshSnapshot, tabId]);

  const setEffort = useCallback(async (level: string) => {
    if (!tabId) return;
    await app.SetRemoteTabEffort(tabId, level);
    await refreshSnapshot();
  }, [refreshSnapshot, tabId]);

  const pauseGoal = useCallback(async () => {
    if (!tabId) return;
    await app.PauseRemoteTabGoal(tabId);
    await refreshSnapshot();
  }, [refreshSnapshot, tabId]);

  const resumeGoal = useCallback(async () => {
    if (!tabId) return;
    await app.ResumeRemoteTabGoal(tabId);
    await refreshSnapshot();
  }, [refreshSnapshot, tabId]);

  const steer = useCallback(async (input: string) => {
    if (!tabId) return;
    await app.SteerRemoteTab(tabId, input);
  }, [tabId]);

  return {
    state, error, transcript, hydrated, running: transcript.running, modelLabel,
    composerProfile, effort, surfaceGeneration, promptError, submit, cancelTurn,
    approve, answer, rewind, setEffort, pauseGoal, resumeGoal, steer,
  };
}
