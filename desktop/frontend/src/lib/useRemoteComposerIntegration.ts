import { useCallback, useEffect, type Dispatch, type MutableRefObject, type SetStateAction } from "react";
import { app } from "./bridge";
import { reconcileComposerProfile, type ComposerProfile, type ComposerProfilesByTab } from "./composerProfile";
import type { GoalAction } from "./goalAction";
import type { CollaborationMode, QualityFloor, RemoteTabRefView, ToolApprovalMode } from "./types";
import type { RemoteSessionApi } from "./useRemoteSession";

type RemoteProfile = RemoteSessionApi["composerProfile"];

export function remoteRuntimeCommand(input: string):
  | { method: "setModel" | "setEffort"; value: string }
  | { method: "newSession" | "clearSession" }
  | undefined {
  const trimmed = input.trim();
  if (trimmed === "/new") return { method: "newSession" };
  if (trimmed === "/clear") return { method: "clearSession" };
  const match = /^\/(model|effort)\s+(\S+)$/.exec(trimmed);
  if (!match) return undefined;
  return { method: match[1] === "model" ? "setModel" : "setEffort", value: match[2] };
}

export function useRemoteComposerSend(
  activeRemote: RemoteTabRefView | undefined,
  activeTabId: string | undefined,
  collaborationMode: CollaborationMode,
  goal: string,
  session: RemoteSessionApi,
  send: (displayText: string, submitText?: string) => Promise<void>,
  applyGoal: (tabId: string, goal: string) => Promise<unknown>,
  requestClear: () => void,
) {
  return useCallback(async (displayText: string, submitText = displayText): Promise<void> => {
    const trimmed = (submitText || displayText).trim();
    const command = remoteRuntimeCommand(trimmed);
    if (command?.method === "clearSession") return requestClear();
    if (command?.method === "newSession") {
      if (!activeRemote) return;
      await app.OpenRemoteProjectTab(activeRemote.hostId, activeRemote.workspace, { newSession: true });
      await session.retryHydration();
      return;
    }
    if (command?.method === "setModel" || command?.method === "setEffort") return session[command.method](command.value);
    if (activeTabId && collaborationMode === "goal" && !goal.trim() && trimmed) await applyGoal(activeTabId, trimmed);
    await send(displayText, submitText);
  }, [activeRemote, activeTabId, applyGoal, collaborationMode, goal, requestClear, send, session]);
}

export function useRemoteComposerProfileSync(options: {
  activeTabId?: string;
  remote: boolean;
  remoteProfile: RemoteProfile;
  collaborationMode: CollaborationMode;
  toolApprovalMode: ToolApprovalMode;
  goal: string;
  qualityFloor: QualityFloor;
  pending: ComposerProfile["pending"];
  setProfiles: Dispatch<SetStateAction<ComposerProfilesByTab>>;
}): boolean {
  const { activeTabId, remote, remoteProfile, collaborationMode, toolApprovalMode, goal, qualityFloor, pending, setProfiles } = options;
  useEffect(() => {
    if (!activeTabId || !remote || !remoteProfile) return;
    setProfiles((current) => {
      const existing = current[activeTabId];
      const backend: ComposerProfile = {
        collaborationMode: remoteProfile.collaborationMode,
        goalDraftMode: false,
        toolApprovalMode: remoteProfile.toolApprovalMode,
        goal: remoteProfile.goal,
        qualityFloor: remoteProfile.qualityFloor,
        pending: {},
      };
      const next = reconcileComposerProfile(existing, backend);
      return existing === next ? current : { ...current, [activeTabId]: next };
    });
  }, [activeTabId, remote, remoteProfile, setProfiles]);

  return !remote || Boolean(remoteProfile
    && (pending.collaborationMode || collaborationMode === remoteProfile.collaborationMode)
    && (pending.toolApprovalMode || toolApprovalMode === remoteProfile.toolApprovalMode)
    && (pending.goal || goal === remoteProfile.goal)
    && (pending.qualityFloor || qualityFloor === remoteProfile.qualityFloor));
}

export function useRemoteComposerRuntimeActions(options: {
  activeTabIdRef: MutableRefObject<string | undefined>;
  remote: boolean;
  session: RemoteSessionApi;
  runGoalAction: (action: GoalAction) => void;
  pauseLocal: (tabId: string) => Promise<unknown>;
  resumeLocal: (tabId: string) => Promise<unknown>;
  setLocalEffort: (level: string) => void;
  showError: (message: string) => void;
}) {
  const { activeTabIdRef, remote, session, runGoalAction, pauseLocal, resumeLocal, setLocalEffort, showError } = options;
  const pauseGoal = useCallback(() => runGoalAction(async () => {
    const tabId = activeTabIdRef.current;
    if (!tabId) return;
    await (remote ? session.pauseGoal() : pauseLocal(tabId));
  }), [activeTabIdRef, pauseLocal, remote, runGoalAction, session]);
  const resumeGoal = useCallback(() => runGoalAction(async () => {
    const tabId = activeTabIdRef.current;
    if (!tabId) return;
    await (remote ? session.resumeGoal() : resumeLocal(tabId));
  }), [activeTabIdRef, remote, resumeLocal, runGoalAction, session]);
  const setEffort = useCallback((level: string) => {
    if (!remote) {
      setLocalEffort(level);
      return;
    }
    void session.setEffort(level).catch((error) => showError(error instanceof Error ? error.message : String(error)));
  }, [remote, session, setLocalEffort, showError]);
  return { pauseGoal, resumeGoal, setEffort };
}
