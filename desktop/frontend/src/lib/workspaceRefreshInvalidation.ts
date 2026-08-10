import { useEffect, useRef } from "react";
import type { WorkspaceRefreshSnapshot } from "./workspaceRefreshStore";
import type { WorkspaceRefreshScheduler } from "./workspaceRefreshScheduler";

type SchedulerRef = { current: WorkspaceRefreshScheduler | null };
type Load = () => Promise<void> | void;

interface WorkspaceRefreshInvalidationOptions {
  filter: string;
  gitMetaSchedulerRef: SchedulerRef;
  loadChangeDetail: Load;
  loadDir: (dir: string) => unknown;
  loadGitHistory: Load;
  loadWorkspaceChanges: Load;
  open: boolean;
  openDirsRef: { current: Set<string> };
  refreshSelected: () => unknown;
  selectedPath: string | null;
  setSearchResults: (value: null) => void;
  viewMode: string;
  workingTreeSchedulerRef: SchedulerRef;
  workspaceRefresh: WorkspaceRefreshSnapshot;
  workspaceScopeKey: string;
}

function parentDirs(path: string): string[] {
  const parts = path.split("/").filter(Boolean);
  const dirs = [""];
  let acc = "";
  for (let i = 0; i < parts.length - 1; i++) {
    acc += `${parts[i]}/`;
    dirs.push(acc);
  }
  return dirs;
}

export function useWorkspaceRefreshInvalidation({
  filter,
  gitMetaSchedulerRef,
  loadChangeDetail,
  loadDir,
  loadGitHistory,
  loadWorkspaceChanges,
  open,
  openDirsRef,
  refreshSelected,
  selectedPath,
  setSearchResults,
  viewMode,
  workingTreeSchedulerRef,
  workspaceRefresh,
  workspaceScopeKey,
}: WorkspaceRefreshInvalidationOptions): void {
  const lastSequenceRef = useRef(workspaceRefresh.sequence);
  const lastRevisionsRef = useRef(workspaceRefresh.revisions);
  const lastScopeRef = useRef(workspaceScopeKey);

  useEffect(() => {
    if (!open) return;
    if (lastScopeRef.current !== workspaceScopeKey) {
      lastScopeRef.current = workspaceScopeKey;
      lastSequenceRef.current = workspaceRefresh.sequence;
      lastRevisionsRef.current = workspaceRefresh.revisions;
      return;
    }
    if (lastSequenceRef.current === workspaceRefresh.sequence) return;
    const previous = lastRevisionsRef.current;
    lastSequenceRef.current = workspaceRefresh.sequence;
    lastRevisionsRef.current = workspaceRefresh.revisions;
    const { changes, revisions } = workspaceRefresh;
    const affectsSelected = workspaceRefresh.allPaths || !selectedPath || changes.some((change) =>
      change.path === selectedPath || change.oldPath === selectedPath || selectedPath.startsWith(`${change.path}/`),
    );
    if (revisions.content > previous.content && affectsSelected && selectedPath) void refreshSelected();
    if (revisions.tree > previous.tree && (workspaceRefresh.allPaths || changes.length > 0)) {
      const affectedDirs = workspaceRefresh.allPaths
        ? openDirsRef.current
        : new Set(changes.flatMap((change) => [change.path, change.oldPath].filter(Boolean).flatMap((path) => parentDirs(path as string))));
      for (const dir of affectedDirs) {
        if (openDirsRef.current.has(dir)) void loadDir(dir);
      }
      if (filter.trim()) setSearchResults(null);
    }
    if (viewMode === "changed") {
      if (revisions.workingTree > previous.workingTree || revisions.session > previous.session) {
        workingTreeSchedulerRef.current?.trigger(async () => {
          await Promise.all([loadWorkspaceChanges(), selectedPath ? loadChangeDetail() : Promise.resolve()]);
        });
      }
      if (revisions.gitMeta > previous.gitMeta) gitMetaSchedulerRef.current?.trigger(loadGitHistory);
    }
    if (workspaceRefresh.watchState !== "active") {
      openDirsRef.current.forEach((dir) => void loadDir(dir));
      if (selectedPath) void refreshSelected();
    }
  }, [
    filter,
    gitMetaSchedulerRef,
    loadChangeDetail,
    loadDir,
    loadGitHistory,
    loadWorkspaceChanges,
    open,
    openDirsRef,
    refreshSelected,
    selectedPath,
    setSearchResults,
    viewMode,
    workingTreeSchedulerRef,
    workspaceRefresh,
    workspaceScopeKey,
  ]);
}
