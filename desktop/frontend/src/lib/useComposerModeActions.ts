import { useCallback, type MutableRefObject } from "react";
import { app } from "./bridge";
import {
  composerProfileWithMode,
  updateUserPlanModeIntent,
  type ComposerProfile,
  type ComposerProfileField,
  type UserPlanModeIntents,
} from "./composerProfile";
import { restorableToolApprovalMode, type RestorableToolApprovalMode } from "./toolApprovalMode";
import { modeHasPlan, type CollaborationMode, type Mode, type ToolApprovalMode } from "./types";

type PatchProfile = (
  patch: Partial<Omit<ComposerProfile, "pending">>,
  pendingFields: ComposerProfileField[],
) => void;

type ComposerModeActionsOptions = {
  activeTabId?: string;
  remote: boolean;
  collaborationMode: CollaborationMode;
  toolApprovalMode: ToolApprovalMode;
  goal: string;
  planIntentRef: MutableRefObject<UserPlanModeIntents>;
  yoloRestoreRef: MutableRefObject<Record<string, RestorableToolApprovalMode>>;
  patchProfile: PatchProfile;
  setControllerMode: (mode: Mode) => Promise<void> | void;
  setControllerCollaborationMode: (mode: CollaborationMode) => Promise<void>;
  setControllerToolApprovalMode: (mode: ToolApprovalMode) => Promise<void> | void;
  clearControllerGoal: () => Promise<void>;
  showError: (message: string) => void;
};

export function useComposerModeActions(options: ComposerModeActionsOptions) {
  const {
    activeTabId, remote, collaborationMode, toolApprovalMode, goal,
    planIntentRef, yoloRestoreRef, patchProfile, setControllerMode,
    setControllerCollaborationMode, setControllerToolApprovalMode,
    clearControllerGoal, showError,
  } = options;
  const rememberPlanMode = useCallback((enabled: boolean) => {
    planIntentRef.current = updateUserPlanModeIntent(planIntentRef.current, activeTabId, enabled);
  }, [activeTabId, planIntentRef]);

  const applyMode = useCallback((mode: Mode) => {
    if (remote && activeTabId) {
      const next = composerProfileWithMode(mode);
      void (async () => {
        try {
          if (goal.trim()) await app.SetRemoteTabGoal(activeTabId, "");
          await app.SetRemoteTabPlanMode(activeTabId, modeHasPlan(mode));
          await app.SetRemoteTabToolApprovalMode(activeTabId, next.toolApprovalMode ?? "ask");
          rememberPlanMode(modeHasPlan(mode));
          patchProfile(next, ["collaborationMode", "toolApprovalMode", "goal"]);
        } catch (error) {
          await Promise.allSettled([
            app.SetRemoteTabGoal(activeTabId, goal),
            app.SetRemoteTabPlanMode(activeTabId, collaborationMode === "plan"),
            app.SetRemoteTabToolApprovalMode(activeTabId, toolApprovalMode),
          ]);
          showError(error instanceof Error ? error.message : String(error));
        }
      })();
      return;
    }
    rememberPlanMode(modeHasPlan(mode));
    patchProfile(composerProfileWithMode(mode), ["collaborationMode", "toolApprovalMode", "goal"]);
    void setControllerMode(mode);
  }, [activeTabId, collaborationMode, goal, patchProfile, rememberPlanMode, remote, setControllerMode, showError, toolApprovalMode]);

  const applyCollaborationMode = useCallback(async (mode: CollaborationMode): Promise<void> => {
    if (remote && activeTabId) {
      if (mode === "goal") {
        await app.SetRemoteTabPlanMode(activeTabId, false);
        rememberPlanMode(false);
        patchProfile({ collaborationMode: "normal", goalDraftMode: true, goal: "" }, ["collaborationMode", "goal"]);
        return;
      }
      if (goal.trim()) await app.SetRemoteTabGoal(activeTabId, "");
      await app.SetRemoteTabPlanMode(activeTabId, mode === "plan");
      rememberPlanMode(mode === "plan");
      patchProfile({ collaborationMode: mode, goalDraftMode: false, goal: "" }, ["collaborationMode", "goal"]);
      return;
    }
    if (mode === "goal") {
      rememberPlanMode(false);
      patchProfile({ collaborationMode: "normal", goalDraftMode: true, goal: "" }, ["collaborationMode", "goal"]);
      return setControllerCollaborationMode("normal");
    }
    if (goal.trim()) await clearControllerGoal();
    await setControllerCollaborationMode(mode);
    rememberPlanMode(mode === "plan");
    patchProfile({ collaborationMode: mode, goalDraftMode: false, goal: "" }, ["collaborationMode", "goal"]);
  }, [activeTabId, clearControllerGoal, goal, patchProfile, rememberPlanMode, remote, setControllerCollaborationMode]);

  const applyToolApprovalMode = useCallback((mode: ToolApprovalMode) => {
    if (!activeTabId) return;
    const rememberRestoreMode = () => {
      if (mode === "yolo" && toolApprovalMode !== "yolo") {
        yoloRestoreRef.current[activeTabId] = restorableToolApprovalMode(toolApprovalMode);
      } else if (mode !== "yolo") {
        yoloRestoreRef.current[activeTabId] = restorableToolApprovalMode(mode);
      }
    };
    if (remote) {
      void app.SetRemoteTabToolApprovalMode(activeTabId, mode).then(() => {
        rememberRestoreMode();
        patchProfile({ toolApprovalMode: mode }, ["toolApprovalMode"]);
      }).catch((error) => showError(error instanceof Error ? error.message : String(error)));
      return;
    }
    rememberRestoreMode();
    patchProfile({ toolApprovalMode: mode }, ["toolApprovalMode"]);
    void setControllerToolApprovalMode(mode);
  }, [activeTabId, patchProfile, remote, setControllerToolApprovalMode, showError, toolApprovalMode, yoloRestoreRef]);

  return { applyMode, applyCollaborationMode, applyToolApprovalMode };
}
