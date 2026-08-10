import { useCallback, useEffect, useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from "react";
import type { Range } from "@tanstack/react-virtual";
import { createSelectionRangeExtractor, type TranscriptSelectionRowRange } from "./transcriptSelectionRange";
import type { TranscriptScrollMode } from "./transcriptScrollController";
import type { TranscriptViewportAnchor } from "./transcriptScrollController";

const SELECTABLE_SELECTOR = "[data-transcript-selectable]";
const ROW_SELECTOR = ".transcript__row[data-row-key]";

function elementForNode(node: Node | null): Element | null {
  if (!node) return null;
  return node.nodeType === Node.ELEMENT_NODE ? node as Element : node.parentElement;
}

function rowKeyForNode(node: Node | null): string | null {
  return elementForNode(node)?.closest<HTMLElement>(ROW_SELECTOR)?.dataset.rowKey ?? null;
}

export function useTranscriptSelectionRetention({
  tabId,
  revealSignal,
  rowIndexByKey,
  setScrollMode,
  cancelStreamingScroll,
  captureViewportAnchor,
  reconcileViewportAnchor,
}: {
  tabId?: string;
  revealSignal: number;
  rowIndexByKey: ReadonlyMap<string, number>;
  setScrollMode: (mode: TranscriptScrollMode, reason?: string) => void;
  cancelStreamingScroll: () => void;
  captureViewportAnchor: () => TranscriptViewportAnchor | null;
  reconcileViewportAnchor: (snapshot: TranscriptViewportAnchor | null) => boolean;
}) {
  const selectionRef = useRef<{ anchorKey: string; focusKey: string; dragging: boolean } | null>(null);
  const [, setRevision] = useState(0);
  const viewportAnchorRef = useRef<TranscriptViewportAnchor | null>(null);

  const publish = useCallback(() => setRevision((value) => value + 1), []);
  const clear = useCallback((reason = "clear") => {
    if (!selectionRef.current) return;
    selectionRef.current = null;
    setScrollMode("manual", reason);
    publish();
  }, [publish, setScrollMode]);

  const onPointerDownCapture = useCallback((event: ReactPointerEvent<HTMLElement>) => {
    if (event.button !== 0) return;
    const target = event.target instanceof Element ? event.target : null;
    const selectable = target?.closest(SELECTABLE_SELECTOR);
    if (!selectable) {
      clear("new-pointer-outside-selection");
      return;
    }
    const anchorKey = selectable.closest<HTMLElement>(ROW_SELECTOR)?.dataset.rowKey;
    if (!anchorKey) return;
    cancelStreamingScroll();
    viewportAnchorRef.current = captureViewportAnchor();
    selectionRef.current = { anchorKey, focusKey: anchorKey, dragging: true };
    setScrollMode("native-selecting", "pointerdown");
    publish();
  }, [cancelStreamingScroll, captureViewportAnchor, clear, publish, setScrollMode]);

  useEffect(() => {
    const onSelectionChange = () => {
      const tracked = selectionRef.current;
      if (!tracked) return;
      const selection = document.getSelection();
      if (!selection || selection.isCollapsed) {
        if (!tracked.dragging) clear("selection-collapsed");
        return;
      }
      const anchorKey = rowKeyForNode(selection.anchorNode);
      const focusKey = rowKeyForNode(selection.focusNode);
      if (!anchorKey || !focusKey) return;
      if (tracked.anchorKey === anchorKey && tracked.focusKey === focusKey) return;
      selectionRef.current = { ...tracked, anchorKey, focusKey };
      publish();
    };
    const finish = (event: PointerEvent) => {
      if (event.button !== 0 || !selectionRef.current?.dragging) return;
      const selection = document.getSelection();
      if (!selection || selection.isCollapsed || selection.toString().trim() === "") {
        clear("empty-pointerup");
        return;
      }
      selectionRef.current = { ...selectionRef.current, dragging: false };
      publish();
      requestAnimationFrame(() => requestAnimationFrame(() => {
        reconcileViewportAnchor(viewportAnchorRef.current);
        viewportAnchorRef.current = null;
        setScrollMode("manual", "native-selection-settled");
      }));
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      document.getSelection()?.removeAllRanges();
      clear("escape");
    };
    document.addEventListener("selectionchange", onSelectionChange);
    document.addEventListener("pointerup", finish);
    document.addEventListener("pointercancel", finish);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("selectionchange", onSelectionChange);
      document.removeEventListener("pointerup", finish);
      document.removeEventListener("pointercancel", finish);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [clear, publish, reconcileViewportAnchor, setScrollMode]);

  useEffect(() => {
    document.getSelection()?.removeAllRanges();
    clear("transcript-reset");
  }, [clear, revealSignal, tabId]);

  useEffect(() => {
    const tracked = selectionRef.current;
    if (!tracked) return;
    if (!rowIndexByKey.has(tracked.anchorKey) || !rowIndexByKey.has(tracked.focusKey)) {
      document.getSelection()?.removeAllRanges();
      clear("selection-endpoint-removed");
    }
  }, [clear, rowIndexByKey]);

  const rangeExtractor = useMemo(() => createSelectionRangeExtractor((): TranscriptSelectionRowRange | null => {
    const tracked = selectionRef.current;
    if (!tracked) return null;
    const anchorIndex = rowIndexByKey.get(tracked.anchorKey);
    const focusIndex = rowIndexByKey.get(tracked.focusKey);
    return anchorIndex == null || focusIndex == null ? null : { anchorIndex, focusIndex };
  }), [rowIndexByKey]);

  return {
    clear,
    onPointerDownCapture,
    rangeExtractor: (range: Range) => rangeExtractor(range),
  };
}
