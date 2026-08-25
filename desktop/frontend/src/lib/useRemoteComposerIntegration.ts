import { useCallback, useEffect, type Dispatch, type MutableRefObject, type SetStateAction } from "react";
import { reconcileComposerProfile, type ComposerProfile, type ComposerProfilesByTab } from "./composerProfile";
import type { GoalAction } from "./goalAction";
import type { CollaborationMode, QualityFloor, ToolApprovalMode } from "./types";
import type { RemoteSessionApi } from "./useRemoteSession";

type RemoteProfile = RemoteSessionApi["composerProfile"];

export function remoteRuntimeCommand(input: string): { method: "setModel" | "setEffort"; value: string } | undefined {
  const match = /^\/(model|effort)\s+(\S+)$/.exec(input.trim());
  if (!match) return undefined;
  return { method: match[1] === "model" ? "setModel" : "setEffort", value: match[2] };
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
