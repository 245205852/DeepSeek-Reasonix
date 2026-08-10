import { useEffect } from "react";
import { tabMetaFallbackDelay } from "./tabMetaRefresh";

type RefreshTabMetas = () => unknown;

export function useWorkspaceFocusReconciliation(
  activeTabId: string | undefined,
  workspaceScopeKey: string,
  refreshTabMetas: RefreshTabMetas,
): void {
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    let focusTimer: number | undefined;
    const schedule = () => {
      if (cancelled) return;
      timer = window.setTimeout(() => {
        void refreshTabMetas();
        schedule();
      }, tabMetaFallbackDelay(document.visibilityState));
    };
    const refreshAndSchedule = (forceVisible = false) => {
      if (timer !== undefined) window.clearTimeout(timer);
      timer = undefined;
      void refreshTabMetas();
      if (activeTabId) void import("./workspaceRefreshStore").then(({ reconcileWorkspaceRefresh }) => reconcileWorkspaceRefresh(activeTabId, workspaceScopeKey, { forceVisible })).catch(() => undefined);
      schedule();
    };
    const requestVisibleRefresh = () => {
      if (cancelled || focusTimer !== undefined) return;
      // Focus and visibility commonly fire together; collapse the pair into
      // one bounded reconciliation for the foreground transition.
      focusTimer = window.setTimeout(() => {
        focusTimer = undefined;
        refreshAndSchedule(true);
      }, 0);
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") requestVisibleRefresh();
      else {
        if (timer !== undefined) window.clearTimeout(timer);
        schedule();
      }
    };
    const onFocus = () => {
      if (document.visibilityState === "visible") requestVisibleRefresh();
    };
    refreshAndSchedule(false);
    document.addEventListener("visibilitychange", onVisibilityChange);
    window.addEventListener("focus", onFocus);
    return () => {
      cancelled = true;
      if (timer !== undefined) window.clearTimeout(timer);
      if (focusTimer !== undefined) window.clearTimeout(focusTimer);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      window.removeEventListener("focus", onFocus);
    };
  }, [activeTabId, refreshTabMetas, workspaceScopeKey]);
}
