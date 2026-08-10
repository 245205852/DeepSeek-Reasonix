import { useCallback, useEffect, useRef, useState } from "react";
import { app } from "./bridge";
import type { HistorySearchHit, SessionMeta } from "./types";

export function useHistoryCatalog({
  isTrash,
  suppliedSessions,
  scope,
  status,
  timeFilter,
  query,
}: {
  isTrash: boolean;
  suppliedSessions: SessionMeta[];
  scope: string;
  status: string;
  timeFilter: string;
  query: string;
}) {
  const [catalogSessions, setCatalogSessions] = useState<SessionMeta[]>(suppliedSessions);
  const [nextCursor, setNextCursor] = useState("");
  const [partial, setPartial] = useState(false);
  const [progress, setProgress] = useState({ indexed: 0, total: 0 });
  const [searchHits, setSearchHits] = useState<HistorySearchHit[]>([]);
  const [refreshNonce, setRefreshNonce] = useState(0);
  const requestSeq = useRef(0);
  const revision = useRef(0);
  const refreshTimer = useRef<number | null>(null);
  const workspaceRoot = suppliedSessions.find((session) => session.current)?.workspaceRoot ?? "";

  useEffect(() => {
    if (isTrash) setCatalogSessions(suppliedSessions);
  }, [isTrash, suppliedSessions]);

  const fetchPage = useCallback(async (cursor: string, append: boolean, seq: number) => {
    const request = { scope, workspaceRoot, status, timeFilter, query: query.trim(), cursor, limit: 50 };
    const [page, bodyPage] = await Promise.all([
      app.ListHistorySessions(request),
      query.trim() ? app.SearchHistoryContent({ ...request, kinds: [], toolName: "" }) : Promise.resolve(null),
    ]);
    if (seq !== requestSeq.current) return;
    if (page.staleCursor && cursor) {
      void fetchPage("", false, seq);
      return;
    }
    setCatalogSessions((current) => append
      ? [...current, ...page.items.filter((item) => !current.some((existing) => existing.path === item.path))]
      : page.items);
    setNextCursor(page.nextCursor);
    setPartial(page.partial || Boolean(bodyPage?.partial));
    if (!append) setSearchHits(bodyPage?.items ?? []);
    if (bodyPage) setProgress({ indexed: bodyPage.status.indexed, total: bodyPage.status.total });
  }, [query, scope, status, timeFilter, workspaceRoot]);

  useEffect(() => {
    if (isTrash || typeof window === "undefined" || !window.runtime) return;
    const seq = ++requestSeq.current;
    const timer = window.setTimeout(() => {
      void fetchPage("", false, seq).catch(() => {
        if (seq !== requestSeq.current) return;
        setCatalogSessions(suppliedSessions);
        setNextCursor("");
        setSearchHits([]);
      });
    }, query.trim() ? 200 : 0);
    return () => window.clearTimeout(timer);
  }, [fetchPage, isTrash, query, refreshNonce, suppliedSessions]);

  useEffect(() => {
    if (isTrash || typeof window === "undefined" || !window.runtime) return;
    const unsubscribe = window.runtime.EventsOn("history-index:changed-v1", (payload?: unknown) => {
      if (!payload || typeof payload !== "object") return;
      const event = payload as { revision?: number; indexed?: number; total?: number; pending?: number };
      const nextRevision = typeof event.revision === "number" ? event.revision : 0;
      if (nextRevision <= revision.current) return;
      revision.current = nextRevision;
      setProgress({ indexed: Number(event.indexed) || 0, total: Number(event.total) || 0 });
      setPartial((Number(event.pending) || 0) > 0 || (Number(event.indexed) || 0) < (Number(event.total) || 0));
      if (refreshTimer.current === null) {
        refreshTimer.current = window.setTimeout(() => {
          refreshTimer.current = null;
          setRefreshNonce((value) => value + 1);
        }, 200);
      }
    });
    return () => {
      unsubscribe();
      if (refreshTimer.current !== null) window.clearTimeout(refreshTimer.current);
      refreshTimer.current = null;
    };
  }, [isTrash]);

  const loadMore = useCallback(() => {
    if (!nextCursor) return;
    void fetchPage(nextCursor, true, requestSeq.current);
  }, [fetchPage, nextCursor]);

  return { sessions: isTrash ? suppliedSessions : catalogSessions, nextCursor, partial, progress, searchHits, loadMore };
}
