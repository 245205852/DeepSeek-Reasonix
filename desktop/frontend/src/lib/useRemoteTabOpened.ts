import { useEffect, type MutableRefObject } from "react";
import { onRemoteTabOpened } from "./bridge";
import type { TabMeta } from "./types";

export function useRemoteTabOpened(
  activeTabIdRef: MutableRefObject<string | undefined>,
  seedActiveTabMeta: (tab: TabMeta) => void,
  switchTab: (tabId: string, tab?: TabMeta) => Promise<unknown>,
) {
  useEffect(() => {
    const off = onRemoteTabOpened((meta) => {
      if (!meta?.id || !meta.remote) return;
      seedActiveTabMeta(meta);
      // Title refreshes reuse this channel and should not remount the surface.
      if (activeTabIdRef.current !== meta.id) void switchTab(meta.id, meta);
    });
    return off;
  }, [activeTabIdRef, seedActiveTabMeta, switchTab]);
}
