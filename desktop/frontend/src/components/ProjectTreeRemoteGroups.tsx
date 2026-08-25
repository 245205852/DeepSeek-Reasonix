import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { Server, Square, XCircle } from "lucide-react";

import { app } from "../lib/bridge";
import type { Translator } from "../lib/i18n";
import type { RemoteSessionView, RemoteTabRefView } from "../lib/types";
import type { ToastContextValue } from "../lib/toast";
import { useRemoteStore, waitForRemoteConnection } from "../store/remote";
import type { ContextMenuItem } from "./ContextMenu";

export function remoteProjectKey(ref: RemoteTabRefView): string {
  return `${ref.hostId}\u0000${ref.workspace}`;
}

export function useRemoteProjectGroups(
  projects: Array<{ remote?: RemoteTabRefView }>,
  showToast: ToastContextValue["showToast"],
) {
  const statuses = useRemoteStore((state) => state.statuses);
  const [sessions, setSessions] = useState<Record<string, RemoteSessionView[]>>({});
  const sessionLoads = useRef(new Map<string, number>());
  const eligibleSessionKeys = useRef(new Set<string>());
  const nextLoad = useRef(0);
  const groupKeys = useMemo(
    () => projects.flatMap((project) => project.remote ? [remoteProjectKey(project.remote)] : []),
    [projects],
  );

  const openRemoteWindow = useCallback(async (ref: RemoteTabRefView) => {
    try {
      const state = statuses[ref.hostId]?.state;
      if (state !== "connected" && state !== "degraded") {
        await app.ConnectRemoteHost(ref.hostId);
        await waitForRemoteConnection(ref.hostId);
      }
      await app.OpenRemoteWorkspace(ref.hostId, ref.workspace);
    } catch (error) {
      showToast(error instanceof Error ? error.message : String(error), "error");
    }
  }, [showToast, statuses]);

  useEffect(() => {
    const eligible = new Set(groupKeys.filter((key) => {
      const state = statuses[key.split("\u0000")[0]]?.state;
      return state === "connected" || state === "degraded";
    }));
    eligibleSessionKeys.current = eligible;
    for (const key of sessionLoads.current.keys()) {
      if (!eligible.has(key)) sessionLoads.current.delete(key);
    }
    setSessions((current) => {
      const next = Object.fromEntries(Object.entries(current).filter(([key]) => eligible.has(key)));
      return Object.keys(next).length === Object.keys(current).length ? current : next;
    });
    for (const key of eligible) {
      if (key in sessions) continue;
      if (sessionLoads.current.has(key)) continue;
      const [hostId, workspace] = key.split("\u0000");
      const load = ++nextLoad.current;
      sessionLoads.current.set(key, load);
      void app.RemoteProjectSessions(hostId, workspace)
        .then((rows) => {
          if (sessionLoads.current.get(key) === load && eligibleSessionKeys.current.has(key)) {
            setSessions((current) => ({ ...current, [key]: rows }));
          }
        })
        .catch(() => {
          if (sessionLoads.current.get(key) === load && eligibleSessionKeys.current.has(key)) {
            setSessions((current) => ({ ...current, [key]: [] }));
          }
        })
        .finally(() => {
          if (sessionLoads.current.get(key) === load) sessionLoads.current.delete(key);
        });
    }
  }, [groupKeys, sessions, statuses]);

  return { openRemoteWindow, remoteSessions: sessions, setRemoteSessions: setSessions, remoteStatuses: statuses };
}

interface RemoteMenuOptions {
  ref: RemoteTabRefView;
  t: Translator;
  closeMenu: () => void;
  openRemoteWindow: (ref: RemoteTabRefView) => Promise<void>;
  setRemoteSessions: Dispatch<SetStateAction<Record<string, RemoteSessionView[]>>>;
  refresh: () => Promise<void>;
  showToast: ToastContextValue["showToast"];
}

export function buildRemoteProjectMenuItems(options: RemoteMenuOptions): ContextMenuItem[] {
  const { ref, t, closeMenu, openRemoteWindow, setRemoteSessions, refresh, showToast } = options;
  const report = (error: unknown) => showToast(error instanceof Error ? error.message : String(error), "error");
  return [
    {
      key: "remote-open-window", icon: <Server size={13} />, label: t("projectTree.remoteOpenWindow"),
      onSelect: () => { closeMenu(); void openRemoteWindow(ref); },
    },
    {
      key: "remote-stop-server", icon: <Square size={13} />, label: t("projectTree.remoteStopServer"),
      onSelect: () => { closeMenu(); void app.StopRemoteServer(ref.hostId, ref.workspace).catch(report); },
    },
    {
      key: "remote-unpin", icon: <XCircle size={13} />, label: t("projectTree.remoteUnpin"),
      onSelect: () => {
        closeMenu();
        void app.RemoveRemoteProject(ref.hostId, ref.workspace).then(() => {
          setRemoteSessions((current) => {
            const next = { ...current };
            delete next[remoteProjectKey(ref)];
            return next;
          });
          void refresh();
        }).catch(report);
      },
    },
  ];
}

export function RemoteProjectSessionRows({ rows, depth }: {
  rows: RemoteSessionView[];
  depth: number;
}) {
  return rows.map((row) => (
    <div
      key={`remote-session:${row.name}`}
      className="project-tree__topic project-tree__topic--project project-tree__topic--remote-static"
      style={{ paddingLeft: 14 + (depth + 1) * 16 }}
    >
      <span className="project-tree__topic-label">{row.title || row.name}</span>
      {row.turns > 0 ? <span className="project-tree__topic-time">{row.turns}</span> : null}
    </div>
  ));
}
